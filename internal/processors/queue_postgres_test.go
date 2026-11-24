package processors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	migrationssql "github.com/spencernorton/DH-Waypoint/migrations"
	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/require"
)

var testContext = context.Background()

func fixture(t *testing.T) (*Store, *pgxpool.Pool, *goose.Provider) { return fixtureAt(t, 0) }
func fixtureAt(t *testing.T, version int64) (*Store, *pgxpool.Pool, *goose.Provider) {
	t.Helper()
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL not configured")
	}
	ctx := testContext
	admin, e := pgx.Connect(ctx, dsn)
	require.NoError(t, e)
	_, e = admin.Exec(ctx, `SELECT pg_advisory_lock(830571);CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;CREATE EXTENSION IF NOT EXISTS btree_gin WITH SCHEMA public;SELECT pg_advisory_unlock(830571)`)
	require.NoError(t, e)
	schema := fmt.Sprintf("processors_%d", time.Now().UnixNano())
	_, e = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, e)
	cfg, e := pgxpool.ParseConfig(dsn)
	require.NoError(t, e)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, e)
	db := stdlib.OpenDB(*cfg.ConnConfig)
	provider, e := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, e)
	if version > 0 {
		_, e = provider.UpTo(ctx, version)
	} else {
		_, e = provider.Up(ctx)
	}
	require.NoError(t, e)
	t.Cleanup(func() {
		db.Close()
		pool.Close()
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		_ = admin.Close(ctx)
	})
	conf := NewDefaultConfig()
	conf.MaxAttempts = 2
	conf.Concurrency = 1
	s, e := NewStore(conf, lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }), "synthetic-core-v1")
	require.NoError(t, e)
	// Tests count whole ticks; under -race a tick can pass the production bound,
	// which scan_bounds_postgres_test.go covers on its own.
	s.scanHold = time.Hour
	return s, pool, provider
}
func seedTorrent(t *testing.T, p *pgxpool.Pool, n byte, kind any) []byte {
	t.Helper()
	h := protocol.InfoHash{n}.Bytes()
	_, e := p.Exec(testContext, `INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at)VALUES($1,'Synthetic.Amber.2024.1080p.WEB-DL.H264-GROUP.mkv',734003200,false,'single',now(),now());
 INSERT INTO torrent_contents(info_hash,content_type,size,created_at,updated_at)VALUES($1,$2,734003200,now(),now());`, h, kind)
	require.NoError(t, e)
	return h
}

type service struct {
	server   *httptest.Server
	identity v1.Identity
	calls    atomic.Int64
}

func newService(t *testing.T, id string, stages []v1.Stage, fn func(v1.Job) v1.Result) *service {
	s := &service{identity: v1.Identity{ID: id, Version: "1.0.0", ModelVersion: "none", PolicyVersion: "test-v1", ArtifactDigest: "none"}}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/manifest":
			_ = json.NewEncoder(w).Encode(v1.Manifest{Contract: v1.Contract, Processor: s.identity, Capabilities: stages, ConfigurationSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`), Qualification: "synthetic"})
		case "/v1/status":
			_ = json.NewEncoder(w).Encode(v1.ServiceStatus{Contract: v1.Contract, Processor: s.identity, State: "ready", InstanceID: "test-instance", StartedAt: "2026-01-01T00:00:00Z"})
		case "/v1/process":
			s.calls.Add(1)
			var j v1.Job
			_ = json.NewDecoder(r.Body).Decode(&j)
			result := fn(j)
			_ = json.NewEncoder(w).Encode(result)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}
func (s *service) write() v1.RegistryWrite {
	return v1.RegistryWrite{Endpoint: s.server.URL, Mode: "apply", State: "active", Configuration: json.RawMessage(`{}`)}
}
func result(j v1.Job, kind string) v1.Result {
	r := v1.Result{Binding: j.Binding, Outcome: v1.Completed, Proposal: &v1.Proposal{ContentType: kind}, Reasons: []string{}, Evidence: []v1.Evidence{}}
	return r
}
func register(t *testing.T, s *Store, p *service) Entry {
	t.Helper()
	x, e := s.Register(testContext, p.identity.ID, p.write())
	require.NoError(t, e)
	return x
}
func run(t *testing.T, s *Store) {
	t.Helper()
	ok, e := s.RunOne(testContext)
	require.NoError(t, e)
	require.True(t, ok)
}

func TestPostgresZeroPluginsAndDeterministicApplication(t *testing.T) {
	s, p, _ := fixture(t)
	h := seedTorrent(t, p, 1, nil)
	n, e := s.Scan(testContext)
	require.NoError(t, e)
	require.Zero(t, n)
	ok, e := s.RunOne(testContext)
	require.NoError(t, e)
	require.False(t, ok)
	status, e := s.Status(testContext)
	require.NoError(t, e)
	require.Empty(t, status.Processors)
	require.Equal(t, "ready", status.CoreReadiness)
	a := newService(t, "alpha", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result {
		// A healthy response may exceed the fault test's transport deadline.
		// Ordinary database work and requests use the production request budget.
		time.Sleep(200 * time.Millisecond)
		return result(j, "movie")
	})
	register(t, s, a)
	n, e = s.Scan(testContext)
	require.NoError(t, e)
	require.Equal(t, 1, n)
	run(t, s)
	var kind, codec, source string
	e = p.QueryRow(testContext, `SELECT content_type,video_codec,video_source FROM torrent_contents WHERE info_hash=$1`, h).Scan(&kind, &codec, &source)
	require.NoError(t, e)
	require.Equal(t, "movie", kind)
	require.Equal(t, "H264", codec)
	require.Equal(t, "WEBDL", source)
	n, e = s.Scan(testContext)
	require.NoError(t, e)
	require.Zero(t, n)
	n, e = s.Scan(testContext)
	require.NoError(t, e)
	require.Zero(t, n)
	require.EqualValues(t, 1, a.calls.Load())
	var apps int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_applications`).Scan(&apps))
	require.Equal(t, 1, apps)
}

func TestPostgresPayloadBackedPriorityAheadOfLinearCatchUp(t *testing.T) {
	s, p, _ := fixture(t)
	verifier := strings.Repeat("b", 64)
	prevVerifier := strings.Repeat("c", 64)
	artifact := strings.Repeat("a", 64)
	svc := newService(t, "synthetic-reviewer", []v1.Stage{v1.Policy}, policyResult)
	svc.identity.ArtifactDigest = artifact
	s.cfg.PayloadVerifierToken = namepolicy.SecretToken(strings.Repeat("verifier", 8))
	register(t, s, svc)
	require.NoError(t, s.SetPublicationConfig(serving.Config{
		RequiredProcessor:                     svc.identity.ID,
		RequiredProcessorArtifactDigest:       artifact,
		RequiredPayloadVerifierArtifactDigest: verifier,
		PreviousPayloadVerifierArtifactDigest: prevVerifier,
	}))

	// Seed 40 torrents (0x01..0x28); place valid unexpired PayloadProof v2 (required) on 0x28
	// and v1 (previous rollover digest) on 0x27, both far behind linear cursor.
	var prevHash, lastHash []byte
	for i := byte(1); i <= 40; i++ {
		h := seedTorrent(t, p, i, "movie")
		if i == 39 {
			prevHash = h
		}
		if i == 40 {
			lastHash = h
		}
	}
	// Clear pending outbox so only linear catch-up and payload-priority compete.
	_, err := p.Exec(testContext, `DELETE FROM processor_publication_pending`)
	require.NoError(t, err)
	_, err = p.Exec(testContext, `INSERT INTO processor_payload_reviews(info_hash,source_revision,verifier_artifact,policy,approved,reason,receipt,expires_at)
 SELECT info_hash,COALESCE(revision,1),$2,$3,true,'decoded_media','{}',now()+interval '1 day'
 FROM torrents LEFT JOIN processor_publication_sources USING(info_hash) WHERE info_hash=$1`, lastHash, verifier, serving.PayloadEvidenceContractV2)
	require.NoError(t, err)
	_, err = p.Exec(testContext, `INSERT INTO processor_payload_reviews(info_hash,source_revision,verifier_artifact,policy,approved,reason,receipt,expires_at)
 SELECT info_hash,COALESCE(revision,1),$2,$3,true,'decoded_media','{}',now()+interval '2 days'
 FROM torrents LEFT JOIN processor_publication_sources USING(info_hash) WHERE info_hash=$1`, prevHash, prevVerifier, serving.PayloadEvidenceContract)
	require.NoError(t, err)

	s.cfg.ScanBatch = 6
	s.cfg.QueueCapacity = 6
	n, err := s.Scan(testContext)
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 3)

	var prioritized int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_jobs WHERE info_hash IN ($1,$2)`, prevHash, lastHash).Scan(&prioritized))
	require.Equal(t, 2, prioritized, "unexpired v2 and rollover-v1 PayloadProof torrents behind scan_after must be prioritized immediately")

	// Verify scan_after did NOT jump to 0x27/0x28 (linear catch-up must not skip 0x04..0x26).
	var after []byte
	require.NoError(t, p.QueryRow(testContext, `SELECT scan_after FROM processor_control WHERE singleton`).Scan(&after))
	require.Less(t, after[0], byte(10), "priority admission must not skip linear scan_after cursor")
}
func TestPostgresOrderDuplicateStaleAndRestart(t *testing.T) {
	s, p, _ := fixture(t)
	h := seedTorrent(t, p, 2, nil)
	a := newService(t, "alpha", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") })
	b := newService(t, "beta", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "tv_show") })
	register(t, s, b)
	register(t, s, a)
	_, e := s.Scan(testContext)
	require.NoError(t, e)
	l, e := s.Claim(testContext)
	require.NoError(t, e)
	require.NotNil(t, l)
	require.Equal(t, "alpha", l.Plan.Steps[0].Entry.ID)
	j, e := s.Prepare(testContext, *l)
	require.NoError(t, e)
	require.NoError(t, s.startAttempt(testContext, *l))
	r := result(j, "movie")
	require.NoError(t, s.Finish(testContext, *l, j, r, true))
	require.ErrorIs(t, s.Finish(testContext, *l, j, r, true), ErrStale)
	run(t, s)
	var kind string
	require.NoError(t, p.QueryRow(testContext, `SELECT content_type FROM torrent_contents WHERE info_hash=$1`, h).Scan(&kind))
	require.Equal(t, "movie", kind)
	seedTorrent(t, p, 3, nil)
	_, e = s.Scan(testContext)
	require.NoError(t, e)
	_, e = s.Scan(testContext)
	require.NoError(t, e)
	l, e = s.Claim(testContext)
	require.NoError(t, e)
	require.NotNil(t, l)
	j, e = s.Prepare(testContext, *l)
	require.NoError(t, e)
	require.NoError(t, s.startAttempt(testContext, *l))
	_, e = p.Exec(testContext, `UPDATE processor_jobs SET lease_until=now()-interval '1 second' WHERE job_id=$1`, l.ID)
	require.NoError(t, e)
	restarted, e := NewStore(s.cfg, s.pool, s.policy)
	require.NoError(t, e)
	again, e := restarted.Claim(testContext)
	require.NoError(t, e)
	require.NotNil(t, again)
	require.Greater(t, again.Generation, l.Generation)
	same, e := restarted.Prepare(testContext, *again)
	require.NoError(t, e)
	require.Equal(t, j.Binding, same.Binding)
	require.ErrorIs(t, s.Finish(testContext, *l, j, result(j, "movie"), true), ErrStale)
	_, e = p.Exec(testContext, `UPDATE torrents SET name='Changed metadata' WHERE info_hash=$1`, again.Hash)
	require.NoError(t, e)
	require.NoError(t, restarted.Finish(testContext, *again, same, result(same, "movie"), false)) // step 0 recorded, final application rechecks again
	_, e = restarted.RunOne(testContext)
	require.ErrorIs(t, e, ErrStale) // stale source is refused before egress
}
func TestPostgresFailuresAreBoundedAndNeverJunk(t *testing.T) {
	for _, behavior := range []string{"timeout", "malformed", "abstain", "defer", "crash"} {
		t.Run(behavior, func(t *testing.T) {
			s, p, _ := fixture(t)
			h := seedTorrent(t, p, 4, "movie")
			a := newService(t, "fault", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result {
				switch behavior {
				case "timeout":
					time.Sleep(250 * time.Millisecond)
				case "malformed":
					r := result(j, "movie")
					r.JobID = strings.Repeat("0", 64)
					return r
				case "abstain":
					return v1.Result{Binding: j.Binding, Outcome: v1.Abstained}
				case "defer":
					return v1.Result{Binding: j.Binding, Outcome: v1.Deferred}
				}
				return result(j, "movie")
			})
			register(t, s, a)
			if behavior == "timeout" {
				// Isolate the deliberately short deadline to HTTP fault injection;
				// it must not expire while preparing the job in PostgreSQL.
				s.transport = NewTransport(150 * time.Millisecond)
			}
			if behavior == "crash" {
				a.server.Close()
			}
			_, e := s.Scan(testContext)
			require.NoError(t, e)
			run(t, s)
			_, e = p.Exec(testContext, `UPDATE processor_jobs SET available_at=now() WHERE state='deferred'`)
			require.NoError(t, e)
			if behavior != "abstain" {
				run(t, s)
			}
			var attempts int
			var state, outcome string
			require.NoError(t, p.QueryRow(testContext, `SELECT attempts,state,outcome FROM processor_jobs`).Scan(&attempts, &state, &outcome))
			require.LessOrEqual(t, attempts, 2)
			require.Equal(t, "completed", state)
			if behavior == "abstain" {
				require.Equal(t, "abstained", outcome)
			} else if behavior == "defer" {
				require.Equal(t, "deferred", outcome)
			} else {
				require.Equal(t, "failed", outcome)
			}
			if behavior == "timeout" {
				require.Equal(t, 2, attempts)
				require.EqualValues(t, 2, a.calls.Load())
				var timedOut int
				require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_attempts WHERE outcome='failed' AND reason='transport_failed'`).Scan(&timedOut))
				require.Equal(t, 2, timedOut)
			}
			var kept int
			require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM torrent_contents WHERE info_hash=$1 AND content_type='movie'`, h).Scan(&kept))
			require.Equal(t, 1, kept)
			require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_exclusions`).Scan(&kept))
			require.Zero(t, kept)
		})
	}
}
func TestPostgresUpgradeRollbackCancellationAndBackpressure(t *testing.T) {
	s, p, _ := fixture(t)
	seedTorrent(t, p, 5, nil)
	seedTorrent(t, p, 6, nil)
	s.cfg.QueueCapacity = 1
	a := newService(t, "versioned", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") })
	old := register(t, s, a)
	n, e := s.Scan(testContext)
	require.NoError(t, e)
	require.Equal(t, 1, n)
	n, e = s.Scan(testContext)
	require.NoError(t, e)
	require.Zero(t, n)
	l, e := s.Claim(testContext)
	require.NoError(t, e)
	j, e := s.Prepare(testContext, *l)
	require.NoError(t, e)
	b := newService(t, "versioned", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "tv_show") })
	b.identity.Version = "2.0.0"
	w := b.write()
	w.ExpectedGeneration = old.Generation
	bad := b.identity
	bad.Version = "wrong"
	w.ExpectedProcessor = &bad
	_, e = s.Register(testContext, b.identity.ID, w)
	require.Error(t, e)
	w.ExpectedProcessor = &b.identity
	newer, e := s.Register(testContext, b.identity.ID, w)
	require.NoError(t, e)
	require.ErrorIs(t, s.Finish(testContext, *l, j, result(j, "movie"), false), ErrStale)
	_, e = s.Register(testContext, b.identity.ID, w)
	require.ErrorIs(t, e, ErrConflict)
	w = a.write()
	w.ExpectedGeneration = newer.Generation
	w.ExpectedProcessor = &a.identity
	rolled, e := s.Register(testContext, a.identity.ID, w)
	require.NoError(t, e)
	require.Greater(t, rolled.ExecutionGeneration, newer.ExecutionGeneration)
	_, e = s.Scan(testContext)
	require.NoError(t, e)
	l, e = s.Claim(testContext)
	require.NoError(t, e)
	require.NotNil(t, l)
	state, e := s.Cancel(testContext, l.ID)
	require.NoError(t, e)
	require.Equal(t, "canceled", state)
	state, e = s.Cancel(testContext, l.ID)
	require.NoError(t, e)
	require.Equal(t, "canceled", state)
	require.NoError(t, s.Remove(testContext, a.identity.ID, rolled.Generation))
	n, e = s.Scan(testContext)
	require.NoError(t, e)
	require.Zero(t, n)
}

func TestLockedSourceBoundsTableLockWait(t *testing.T) {
	s, p, _ := fixture(t)
	h := seedTorrent(t, p, 41, "movie")
	writer, e := p.Begin(testContext)
	require.NoError(t, e)
	defer rollback(writer)
	// The locked read takes per-hash row locks, so contention is a held
	// parent row lock rather than a table lock.
	_, e = writer.Exec(testContext, `SELECT info_hash FROM torrents WHERE info_hash=$1 FOR UPDATE`, h)
	require.NoError(t, e)
	tx, e := s.begin(testContext)
	require.NoError(t, e)
	defer rollback(tx)
	bounded, cancel := context.WithTimeout(testContext, 10*time.Second)
	defer cancel()
	start := time.Now()
	_, _, e = readSource(bounded, tx, h, true)
	var pgErr *pgconn.PgError
	require.ErrorAs(t, e, &pgErr)
	require.Equal(t, "55P03", pgErr.Code)
	require.Less(t, time.Since(start), 5*time.Second)
	rollback(writer)
	locked, e := s.begin(testContext)
	require.NoError(t, e)
	defer rollback(locked)
	_, _, e = readSource(testContext, locked, h, true)
	require.NoError(t, e)
	var timeout string
	require.NoError(t, locked.QueryRow(testContext, `SHOW lock_timeout`).Scan(&timeout))
	require.Equal(t, "0", timeout)
}

func TestLockedSourceAllowsUnrelatedEvidenceInsert(t *testing.T) {
	s, p, _ := fixture(t)
	h := seedTorrent(t, p, 42, "movie")
	other := seedTorrent(t, p, 43, "movie")
	locked, e := s.begin(testContext)
	require.NoError(t, e)
	defer rollback(locked)
	_, _, e = readSource(testContext, locked, h, true)
	require.NoError(t, e)
	// While the locked read holds h, an evidence insert for an unrelated
	// hash must not wait on it. The bounded context turns a table-wide
	// lock regression into a failure instead of a hang.
	insertCtx, cancel := context.WithTimeout(testContext, 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, e = p.Exec(insertCtx, `INSERT INTO label_evidence(source,source_kind,source_instance,source_object_id,info_hash,observed_at,strength) VALUES('sonarr','synthetic','fixture','unrelated-evidence',$1,now(),10)`, other)
	require.NoError(t, e)
	require.Less(t, time.Since(start), 1500*time.Millisecond)
	var n int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM label_evidence WHERE info_hash=$1`, other).Scan(&n))
	require.Equal(t, 1, n)
}

func TestPostgresPayloadPriorityBoundedWindowCursorBackoffAndRoundRobin(t *testing.T) {
	s, p, _ := fixture(t)
	verifier := strings.Repeat("d", 64)
	prevVerifier := strings.Repeat("e", 64)
	artifact := strings.Repeat("f", 64)
	svc := newService(t, "synthetic-reviewer", []v1.Stage{v1.Policy}, policyResult)
	svc.identity.ArtifactDigest = artifact
	s.cfg.PayloadVerifierToken = namepolicy.SecretToken(strings.Repeat("verifier", 8))
	register(t, s, svc)
	require.NoError(t, s.SetPublicationConfig(serving.Config{
		RequiredProcessor:                     svc.identity.ID,
		RequiredProcessorArtifactDigest:       artifact,
		RequiredPayloadVerifierArtifactDigest: verifier,
		PreviousPayloadVerifierArtifactDigest: prevVerifier,
	}))

	// 1. Seed 72 torrents where the first 70 (0x01..0x46) already hold current
	//    publication reviews (rejected prefix exceeding payloadPriorityWindow=64),
	//    followed by eligible #71 (0xfe) and #72 (0xff, testing 0xff leading byte
	//    and equal-expires_at tie breaking!).
	baseExp := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	var rejectedHashes [][]byte
	for i := byte(1); i <= 70; i++ {
		h := seedTorrent(t, p, i, "movie")
		rejectedHashes = append(rejectedHashes, h)
		_, err := p.Exec(testContext, `INSERT INTO processor_payload_reviews(info_hash,source_revision,verifier_artifact,policy,approved,reason,receipt,expires_at)
 SELECT info_hash,COALESCE(revision,1),$2,$3,true,'decoded_media','{}',$4
 FROM torrents LEFT JOIN processor_publication_sources USING(info_hash) WHERE info_hash=$1`,
			h, verifier, serving.PayloadEvidenceContractV2, baseExp.Add(time.Duration(i)*time.Second))
		require.NoError(t, err)
		_, err = p.Exec(testContext, `INSERT INTO processor_publication_reviews(info_hash,processor_id,source_revision,execution_generation,processor_identity,approved,policy_contract,reason,job_id)
 SELECT info_hash,$2,COALESCE(revision,1),(SELECT execution_generation FROM processor_registry WHERE id=$2),'{"name":"synthetic-reviewer"}'::jsonb,true,$3,'ok',$4
 FROM torrents LEFT JOIN processor_publication_sources USING(info_hash) WHERE info_hash=$1`,
			h, svc.identity.ID, serving.PublicationReviewPolicy, fmt.Sprintf("pre-reviewed-%02x", i))
		require.NoError(t, err)
	}
	elig71 := seedTorrent(t, p, 0xfe, "movie")
	elig72 := seedTorrent(t, p, 0xff, "movie")
	tieExp := baseExp.Add(90 * time.Second)
	for _, h := range [][]byte{elig71, elig72} {
		_, err := p.Exec(testContext, `INSERT INTO processor_payload_reviews(info_hash,source_revision,verifier_artifact,policy,approved,reason,receipt,expires_at)
 SELECT info_hash,COALESCE(revision,1),$2,$3,true,'decoded_media','{}',$4
 FROM torrents LEFT JOIN processor_publication_sources USING(info_hash) WHERE info_hash=$1`,
			h, verifier, serving.PayloadEvidenceContractV2, tieExp)
		require.NoError(t, err)
	}
	// Also put one eligible torrent in the rollover previous-verifier v1 partition
	// to verify round-robin across partitions!
	rolloverElig := seedTorrent(t, p, 0xdd, "movie")
	_, err := p.Exec(testContext, `INSERT INTO processor_payload_reviews(info_hash,source_revision,verifier_artifact,policy,approved,reason,receipt,expires_at)
 SELECT info_hash,COALESCE(revision,1),$2,$3,true,'decoded_media','{}',$4
 FROM torrents LEFT JOIN processor_publication_sources USING(info_hash) WHERE info_hash=$1`,
		rolloverElig, prevVerifier, serving.PayloadEvidenceContract, baseExp.Add(10*time.Second))
	require.NoError(t, err)

	// Clear pending outbox so only payload priority + linear catch-up run.
	_, err = p.Exec(testContext, `DELETE FROM processor_publication_pending`)
	require.NoError(t, err)

	// Scan 1: partition 0 visits rows 1..64 (all rejected -> advances cursor past 64),
	// and partition 3 (rollover v1) finds rolloverElig immediately!
	s.cfg.ScanBatch = 6
	s.cfg.QueueCapacity = 20
	_, err = s.Scan(testContext)
	require.NoError(t, err)

	var rollQueued int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_jobs WHERE info_hash=$1`, rolloverElig).Scan(&rollQueued))
	require.Equal(t, 1, rollQueued, "rollover partition must be admitted even when partition 0 hits a 64-row rejected prefix")

	// Scan 2: partition 0 resumes at row 65..72 -> admits BOTH equal-expires_at eligible
	// rows (0xff and 0xfe) without starvation!
	_, err = s.Scan(testContext)
	require.NoError(t, err)

	var tailQueued int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_jobs WHERE info_hash IN ($1,$2)`, elig71, elig72).Scan(&tailQueued))
	require.Equal(t, 2, tailQueued, "eligible rows behind a >64 rejected prefix (including 0xff tie) must be admitted on the next bounded window")

	// Partition 0 reached EOF on Scan 2 -> drainedUntil backoff must be armed!
	require.Nil(t, s.discoverPayloadPriority(testContext), "all drained partitions must skip DB window queries during drainedUntil backoff")

	// Generation rollover (bumped execution_generation) must invalidate stale drainedUntil immediately!
	_, err = p.Exec(testContext, `UPDATE processor_registry SET execution_generation = execution_generation + 1 WHERE id = $1`, svc.identity.ID)
	require.NoError(t, err)
	discAfterRollover := s.discoverPayloadPriority(testContext)
	require.NotNil(t, discAfterRollover, "generation rollover must bypass previous-generation drainedUntil backoff")
	require.NotEmpty(t, discAfterRollover.candidates)
}

// Keep generated model codecs exercised in the source snapshot path.
var _ = model.Torrent{}
