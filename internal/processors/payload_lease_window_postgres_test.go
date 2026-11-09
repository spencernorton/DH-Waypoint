package processors

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/require"
)

// The plan expects ~50 ms; the margin absorbs -race and a shared test
// database, while a wait on a held lock would take seconds.
const lwPrompt = 500 * time.Millisecond

// lwLocks are row locks another session can hold on a candidate's source.
var lwLocks = []struct{ name, sql string }{
	{"torrent", `SELECT 1 FROM torrents WHERE info_hash=$1 FOR UPDATE`},
	{"contents", `SELECT 1 FROM torrent_contents WHERE info_hash=$1 FOR UPDATE`},
	{"source", `SELECT 1 FROM processor_publication_sources WHERE info_hash=$1 FOR UPDATE`},
}

// lwSetup is the existing payload fixture: a required synthetic reviewer
// and a configured payload verifier.
func lwSetup(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	s, p, _ := fixture(t)
	s.cfg.PayloadVerifierToken = namepolicy.SecretToken(strings.Repeat("verifier", 8))
	svc := newService(t, "synthetic-reviewer", []v1.Stage{v1.Policy}, policyResult)
	svc.identity.ArtifactDigest = strings.Repeat("a", 64)
	register(t, s, svc)
	require.NoError(t, s.SetPublicationConfig(serving.Config{RequiredProcessor: svc.identity.ID, RequiredProcessorArtifactDigest: svc.identity.ArtifactDigest, RequiredPayloadVerifierArtifactDigest: strings.Repeat("b", 64)}))
	return s, p
}

func lwHash(i int) []byte {
	h := make([]byte, 20)
	copy(h, "lw")
	binary.BigEndian.PutUint32(h[16:], uint32(i))
	return h
}

// lwRows seeds n synthetic torrents with contents, numbered from first, and
// queues them with lwQueue. It returns the hashes in queue order.
func lwRows(t *testing.T, s *Store, p *pgxpool.Pool, first, n, priority int) [][]byte {
	t.Helper()
	hashes := make([][]byte, n)
	for i := range hashes {
		hashes[i] = lwHash(first + i)
	}
	_, err := p.Exec(testContext, `INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at)
 SELECT h,'Synthetic.Lease.2024.1080p.WEB-DL.H264-GROUP.mkv',734003200,false,'single',now()-interval '2 days',now() FROM unnest($1::bytea[]) h;
 INSERT INTO torrent_contents(info_hash,content_type,size,created_at,updated_at) SELECT h,'movie',734003200,now(),now() FROM unnest($1::bytea[]) h`, hashes)
	require.NoError(t, err)
	lwQueue(t, s, p, priority, first, hashes...)
	return hashes
}

// lwQueue approves hashes by the required processor at their current
// publication source revision and queues them at priority, enqueued in
// argument order offset milliseconds into a fixed synthetic epoch.
func lwQueue(t *testing.T, s *Store, p *pgxpool.Pool, priority, offset int, hashes ...[]byte) {
	t.Helper()
	tag, err := p.Exec(testContext, `INSERT INTO processor_publication_reviews(info_hash,processor_id,source_revision,execution_generation,processor_identity,approved,reason,job_id,policy_contract)
 SELECT src.info_hash,r.id,src.revision,r.execution_generation,r.manifest->'processor',true,'approved',repeat('a',64),$2
 FROM processor_publication_sources src JOIN processor_registry r ON r.id=$3 WHERE src.info_hash=ANY($1::bytea[])
 ON CONFLICT(info_hash,processor_id) DO UPDATE SET source_revision=EXCLUDED.source_revision,approved=true;
 INSERT INTO processor_payload_pending(info_hash,source_revision,priority,enqueued_at)
 SELECT u.h,src.revision,$4,timestamptz '2026-01-01 00:00:00+00'+($5+u.n)*interval '1 millisecond'
 FROM unnest($1::bytea[]) WITH ORDINALITY u(h,n) JOIN processor_publication_sources src ON src.info_hash=u.h
 ON CONFLICT(info_hash) DO UPDATE SET source_revision=EXCLUDED.source_revision,priority=EXCLUDED.priority,enqueued_at=EXCLUDED.enqueued_at`,
		hashes, serving.PublicationReviewPolicy, s.publication.RequiredProcessor, priority, offset)
	require.NoError(t, err)
	require.EqualValues(t, len(hashes), tag.RowsAffected())
}

// lwOldModel makes the required processor's approvals look like an older
// model artifact's, which the lease window must drop.
func lwOldModel(t *testing.T, p *pgxpool.Pool, hashes ...[]byte) {
	t.Helper()
	_, err := p.Exec(testContext, `UPDATE processor_publication_reviews SET processor_identity=processor_identity||jsonb_build_object('version','0.9.0','artifact_digest',repeat('c',64)) WHERE info_hash=ANY($1::bytea[])`, hashes)
	require.NoError(t, err)
}

// lwWork gives a queued row a payload work row; offset shifts its revision.
func lwWork(t *testing.T, p *pgxpool.Pool, h []byte, offset int, artifact, leaseUntil, availableAt string) {
	t.Helper()
	tag, err := p.Exec(testContext, `INSERT INTO processor_payload_work(info_hash,source_revision,verifier_artifact,lease_id,lease_until,available_at)
 SELECT info_hash,source_revision+$2,$3,repeat('e',64),now()+$4::interval,now()+$5::interval FROM processor_payload_pending WHERE info_hash=$1`, h, offset, artifact, leaseUntil, availableAt)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())
}

// lwPending reports, per hash, whether its payload pending row exists.
func lwPending(t *testing.T, p *pgxpool.Pool, hashes ...[]byte) []bool {
	t.Helper()
	rows, err := p.Query(testContext, `SELECT EXISTS(SELECT 1 FROM processor_payload_pending q WHERE q.info_hash=u.h) FROM unnest($1::bytea[]) WITH ORDINALITY u(h,n) ORDER BY n`, hashes)
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, pgx.RowTo[bool])
	require.NoError(t, err)
	return out
}

// lwWritten returns processor_control.payload_leases and the work row count.
func lwWritten(t *testing.T, p *pgxpool.Pool) (leases, work int64) {
	t.Helper()
	require.NoError(t, p.QueryRow(testContext, `SELECT payload_leases,(SELECT count(*) FROM processor_payload_work) FROM processor_control`).Scan(&leases, &work))
	return leases, work
}

// lwClassify runs the lease's classification over priority 1 with processor
// as the required processor id and returns the labels by hex hash.
func lwClassify(t *testing.T, s *Store, p *pgxpool.Pool, processor string) map[string]string {
	t.Helper()
	rows, err := p.Query(testContext, payloadClassify, 1, time.Time{}, []byte{}, payloadWalkBudget, processor, s.publication.RequiredProcessorArtifactDigest,
		s.publication.RequiredPayloadVerifierArtifactDigest, PayloadEvidenceContract, serving.PublicationReviewPolicy)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]string{}
	n := 0
	for rows.Next() {
		var h []byte
		var revision int64
		var at time.Time
		var class *string
		require.NoError(t, rows.Scan(&h, &revision, &at, &class))
		require.NotNil(t, class, "every row gets a label")
		out[hex.EncodeToString(h)] = *class
		n++
	}
	require.NoError(t, rows.Err())
	require.Len(t, out, n, "one label per row")
	return out
}

// lwHold opens another session's transaction; it holds its locks until it
// is rolled back or the test ends.
func lwHold(t *testing.T, p *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := p.Begin(testContext)
	require.NoError(t, err)
	t.Cleanup(func() { rollback(tx) })
	return tx
}

func lwLock(t *testing.T, tx pgx.Tx, sql string, h []byte) {
	t.Helper()
	tag, err := tx.Exec(testContext, sql, h)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())
}

// lwLease times one lease. The deadline turns a lock wait into a failure
// instead of a hang.
func lwLease(s *Store) (payloadLease, time.Duration, error) {
	ctx, cancel := context.WithTimeout(testContext, 5*time.Second)
	defer cancel()
	start := time.Now()
	l, err := s.leasePayload(ctx)
	return l, time.Since(start), err
}

// lwHook sees every statement a traced store sends, before it is sent.
type lwHook func(conn *pgx.Conn, sql string)

func (h lwHook) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	h(conn, data.SQL)
	return ctx
}

func (lwHook) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// lwTracedStore is a second store on s's schema and configuration whose
// connections run hook first, so a test can stop or cancel a lease at an
// exact statement.
func lwTracedStore(t *testing.T, s *Store, p *pgxpool.Pool, hook lwHook) *Store {
	t.Helper()
	cfg := p.Config()
	cfg.ConnConfig.Tracer = hook
	pool, err := pgxpool.NewWithConfig(testContext, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	traced, err := NewStore(s.cfg, lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }), s.policy)
	require.NoError(t, err)
	require.NoError(t, traced.SetPublicationConfig(s.publication))
	return traced
}

// lwBlockedBy waits until a backend is blocked by pid's locks and returns
// its statement. done reports a waiter that finished without blocking.
func lwBlockedBy(t *testing.T, p *pgxpool.Pool, pid uint32, done <-chan error) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var query string
		// Only control-row statements: other test schemas' migrations in the
		// same database (CREATE INDEX CONCURRENTLY) can also wait on the lease.
		err := p.QueryRow(testContext, `SELECT query FROM pg_stat_activity WHERE $1::int=ANY(pg_blocking_pids(pid)) AND query LIKE '%processor_control%' LIMIT 1`, int32(pid)).Scan(&query)
		if err == nil {
			return query
		}
		require.ErrorIs(t, err, pgx.ErrNoRows)
		select {
		case e := <-done:
			t.Fatalf("finished without waiting on the lease: %v", e)
		default:
		}
		require.True(t, time.Now().Before(deadline), "nothing waited on the lease's locks")
		time.Sleep(time.Millisecond) // poll interval only; nothing is asserted about time
	}
}

func lwRecv(t *testing.T, c <-chan error) error {
	t.Helper()
	select {
	case err := <-c:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("call did not return")
		return nil
	}
}

func TestLeaseWindowDropsOldModelHeadAndLeasesNextEligible(t *testing.T) {
	s, p := lwSetup(t)
	rows := lwRows(t, s, p, 1, 3, 1)
	lwOldModel(t, p, rows[0])
	lease, err := s.leasePayload(testContext)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(rows[1]), lease.InfoHash, "the first eligible row behind the dropped head")
	require.Equal(t, []bool{false, true, true}, lwPending(t, p, rows...), "only the old-model head is dropped")
	leases, work := lwWritten(t, p)
	require.EqualValues(t, 1, leases)
	require.EqualValues(t, 1, work)
}

func TestLeaseWindowKeepsBusyRows(t *testing.T) {
	s, p := lwSetup(t)
	rows := lwRows(t, s, p, 1, 5, 1)
	verifier := s.publication.RequiredPayloadVerifierArtifactDigest
	lwWork(t, p, rows[0], 0, verifier, "1 minute", "1 minute")                // leased at this revision and artifact: busy
	lwWork(t, p, rows[1], 0, verifier, "-1 second", "5 minutes")              // lease over, not yet available: busy
	lwWork(t, p, rows[2], -1, verifier, "1 minute", "1 minute")               // leased for an older revision: eligible
	lwWork(t, p, rows[3], 0, strings.Repeat("c", 64), "1 minute", "1 minute") // leased for another verifier: eligible
	for _, want := range rows[2:] {
		lease, err := s.leasePayload(testContext)
		require.NoError(t, err)
		require.Equal(t, hex.EncodeToString(want), lease.InfoHash)
	}
	_, err := s.leasePayload(testContext)
	require.ErrorIs(t, err, pgx.ErrNoRows, "only busy rows are left")
	require.Equal(t, []bool{true, true, true, true, true}, lwPending(t, p, rows...), "busy rows are kept")
	leases, _ := lwWritten(t, p)
	require.EqualValues(t, 3, leases)
}

func TestLeaseWindowGuardFailureDropsNothing(t *testing.T) {
	for _, c := range []struct{ name, change, undo string }{
		{"paused", `UPDATE processor_registry SET state='paused'`, `UPDATE processor_registry SET state='active'`},
		{"shadow", `UPDATE processor_registry SET mode='shadow'`, `UPDATE processor_registry SET mode='apply'`},
		{"artifact", `UPDATE processor_registry SET manifest=jsonb_set(manifest,'{processor,artifact_digest}',to_jsonb(repeat('d',64)))`,
			`UPDATE processor_registry SET manifest=jsonb_set(manifest,'{processor,artifact_digest}',to_jsonb(repeat('a',64)))`},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, p := lwSetup(t)
			rows := lwRows(t, s, p, 1, 2, 1)
			lwOldModel(t, p, rows[0])
			_, err := p.Exec(testContext, c.change)
			require.NoError(t, err)
			_, err = s.leasePayload(testContext)
			require.ErrorIs(t, err, pgx.ErrNoRows)
			require.Equal(t, []bool{true, true}, lwPending(t, p, rows...), "nothing is dropped while the registry and configuration disagree")
			leases, work := lwWritten(t, p)
			require.Zero(t, leases)
			require.Zero(t, work)
			// Control: once they agree again, the same rows drop and lease.
			_, err = p.Exec(testContext, c.undo)
			require.NoError(t, err)
			lease, err := s.leasePayload(testContext)
			require.NoError(t, err)
			require.Equal(t, hex.EncodeToString(rows[1]), lease.InfoHash)
			require.Equal(t, []bool{false, true}, lwPending(t, p, rows...))
		})
	}
}

func TestLeaseWindowBudgetEndsInsideFirstPriority(t *testing.T) {
	s, p := lwSetup(t)
	low := lwRows(t, s, p, 0, 1, 0)[0] // queued first: only the priority rule puts it last
	old := lwRows(t, s, p, 1, payloadWalkBudget+88, 1)
	lwOldModel(t, p, old...)
	next := lwRows(t, s, p, 1000, 1, 1)[0]
	_, err := s.leasePayload(testContext)
	require.ErrorIs(t, err, pgx.ErrNoRows, "an exhausted budget answers 204, never a lower priority")
	want := make([]bool, len(old))
	for i := payloadWalkBudget; i < len(old); i++ {
		want[i] = true
	}
	require.Equal(t, want, lwPending(t, p, old...), "the visited drops are committed")
	require.Equal(t, []bool{true, true}, lwPending(t, p, next, low))
	leases, work := lwWritten(t, p)
	require.Zero(t, leases, "payload_leases does not move")
	require.Zero(t, work)
	lease, err := s.leasePayload(testContext)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(next), lease.InfoHash, "the next call drops the rest and reaches the eligible row")
	require.NotContains(t, lwPending(t, p, old...), true)
}

func TestLeaseWindowKeepsPriorityAlternation(t *testing.T) {
	s, p := lwSetup(t)
	low := lwRows(t, s, p, 1, 4, 0) // queued first: only the priority rule orders them
	high := lwRows(t, s, p, 100, 8, 1)
	// Historical rule: payload_leases%4 != 3 takes priority 1 first.
	want := [][]byte{high[0], high[1], high[2], low[0], high[3], high[4], high[5], low[1]}
	for i, h := range want {
		lease, err := s.leasePayload(testContext)
		require.NoError(t, err)
		require.Equal(t, hex.EncodeToString(h), lease.InfoHash, "lease %d", i)
	}
	leases, _ := lwWritten(t, p)
	require.EqualValues(t, len(want), leases)
}

func TestLeaseWindowClassificationIsTotalOnNullHeavyRows(t *testing.T) {
	s, p := lwSetup(t)
	rows := lwRows(t, s, p, 1, 3, 1)
	// No review; the required processor's review at another revision; no
	// publication source row. No row has a work row.
	_, err := p.Exec(testContext, `DELETE FROM processor_publication_reviews WHERE info_hash=$1;
 UPDATE processor_publication_reviews SET source_revision=source_revision-1 WHERE info_hash=$2;
 DELETE FROM processor_publication_sources WHERE info_hash=$3`, rows[0], rows[1], rows[2])
	require.NoError(t, err)
	// A current approval whose torrent row is gone: only the source read can
	// tell. A processor with no registry row reviewed it too.
	gone := lwHash(9)
	_, err = p.Exec(testContext, `INSERT INTO processor_publication_sources(info_hash,revision) VALUES($1,1)`, gone)
	require.NoError(t, err)
	lwQueue(t, s, p, 1, 9, gone)
	_, err = p.Exec(testContext, `INSERT INTO processor_publication_reviews(info_hash,processor_id,source_revision,execution_generation,processor_identity,approved,reason,job_id,policy_contract)
 SELECT info_hash,'synthetic-unregistered',source_revision,execution_generation,processor_identity,true,reason,job_id,policy_contract FROM processor_publication_reviews WHERE info_hash=$1`, gone)
	require.NoError(t, err)
	h := hex.EncodeToString
	require.Equal(t, map[string]string{h(rows[0]): "drop", h(rows[1]): "drop", h(rows[2]): "drop", h(gone): "eligible"}, lwClassify(t, s, p, s.publication.RequiredProcessor))
	require.Equal(t, map[string]string{h(rows[0]): "drop", h(rows[1]): "drop", h(rows[2]): "drop", h(gone): "drop"}, lwClassify(t, s, p, "synthetic-unregistered"))
	_, err = s.leasePayload(testContext)
	require.ErrorIs(t, err, pgx.ErrNoRows, "none of them leases")
	require.Equal(t, []bool{false, false, false, true}, lwPending(t, p, rows[0], rows[1], rows[2], gone))
	leases, work := lwWritten(t, p)
	require.Zero(t, leases)
	require.Zero(t, work)
}

func TestLeaseWindowIgnoresAnotherProcessorsRejection(t *testing.T) {
	s, p := lwSetup(t)
	register(t, s, newService(t, "synthetic-second", []v1.Stage{v1.Policy}, policyResult))
	rows := lwRows(t, s, p, 1, 2, 1)
	tag, err := p.Exec(testContext, `INSERT INTO processor_publication_reviews(info_hash,processor_id,source_revision,execution_generation,processor_identity,approved,reason,job_id,policy_contract)
 SELECT q.info_hash,r.id,q.source_revision,r.execution_generation,r.manifest->'processor',false,'policy_not_approved',repeat('b',64),$2
 FROM processor_payload_pending q JOIN processor_registry r ON r.id='synthetic-second' WHERE q.info_hash=ANY($1::bytea[])`, rows, serving.PublicationReviewPolicy)
	require.NoError(t, err)
	require.EqualValues(t, 2, tag.RowsAffected())
	for _, want := range rows {
		lease, err := s.leasePayload(testContext)
		require.NoError(t, err)
		require.Equal(t, hex.EncodeToString(want), lease.InfoHash)
	}
	require.Equal(t, []bool{true, true}, lwPending(t, p, rows...), "neither row is dropped")
}

func TestLeaseWindowSkipsDropRowHeldByAnotherSession(t *testing.T) {
	s, p := lwSetup(t)
	rows := lwRows(t, s, p, 1, 2, 1)
	lwOldModel(t, p, rows[0])
	holder := lwHold(t, p)
	lwLock(t, holder, `DELETE FROM processor_payload_pending WHERE info_hash=$1`, rows[0])
	lease, took, err := lwLease(s)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(rows[1]), lease.InfoHash)
	require.Less(t, took, lwPrompt)
	_, took, err = lwLease(s)
	require.ErrorIs(t, err, pgx.ErrNoRows, "the held drop is skipped and the leased row is busy")
	require.Less(t, took, lwPrompt)
	rollback(holder)
	require.Equal(t, []bool{true, true}, lwPending(t, p, rows...), "the lease never deleted the held row")
	_, err = s.leasePayload(testContext)
	require.ErrorIs(t, err, pgx.ErrNoRows)
	require.Equal(t, []bool{false, true}, lwPending(t, p, rows...), "the next call drops it")
}

func TestLeaseWindowRecheckFailureDeletesRowAndMovesOn(t *testing.T) {
	s, p := lwSetup(t)
	rows := lwRows(t, s, p, 1, 2, 1)
	// The head turns private and is re-approved at its new revision, so only
	// the source recheck can refuse it.
	_, err := p.Exec(testContext, `UPDATE torrents SET private=true WHERE info_hash=$1`, rows[0])
	require.NoError(t, err)
	lwQueue(t, s, p, 1, 0, rows[0])
	lease, err := s.leasePayload(testContext)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(rows[1]), lease.InfoHash)
	require.Equal(t, []bool{false, true}, lwPending(t, p, rows...), "the refused head is deleted, as before")
}

func TestLeaseDropRaceWithFinishReapprovalAtSameRevision(t *testing.T) {
	s, p := lwSetup(t)
	h := seedTorrent(t, p, 7, "movie")
	n, err := s.Scan(testContext)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	run(t, s)
	var revision int64
	var enqueued time.Time
	require.NoError(t, p.QueryRow(testContext, `SELECT source_revision,enqueued_at FROM processor_payload_pending WHERE info_hash=$1`, h).Scan(&revision, &enqueued))
	// An execution-generation bump leaves the approval stale at the same
	// revision: the lease drops the row until a new review re-approves it.
	_, err = p.Exec(testContext, `UPDATE processor_registry SET execution_generation=execution_generation+1;UPDATE processor_control SET scan_after=''::bytea`)
	require.NoError(t, err)
	n, err = s.Scan(testContext)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	l, err := s.Claim(testContext)
	require.NoError(t, err)
	require.NotNil(t, l)
	j, err := s.Prepare(testContext, *l)
	require.NoError(t, err)
	require.NoError(t, s.startAttempt(testContext, *l))

	// Stop the lease after it has classified the row as a drop and before
	// it deletes it, while it holds the control row FOR UPDATE.
	paused, resume := make(chan uint32, 1), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	var once sync.Once
	leaser := lwTracedStore(t, s, p, func(conn *pgx.Conn, sql string) {
		if strings.HasPrefix(sql, "DELETE FROM processor_payload_pending WHERE info_hash IN (") {
			once.Do(func() { paused <- conn.PgConn().PID(); <-resume })
		}
	})
	leased := make(chan error, 1)
	go func() { _, e := leaser.leasePayload(testContext); leased <- e }()
	var pid uint32
	select {
	case pid = <-paused:
	case e := <-leased:
		t.Fatalf("the lease ended without a drop: %v", e)
	case <-time.After(10 * time.Second):
		t.Fatal("the lease never reached its drop")
	}
	finished := make(chan error, 1)
	go func() { finished <- s.Finish(testContext, *l, j, policyResult(j), true) }()
	// Finish's apply waits for the control row the lease holds.
	require.Contains(t, lwBlockedBy(t, p, pid, finished), "FROM processor_control WHERE singleton FOR SHARE")
	release()
	require.ErrorIs(t, lwRecv(t, leased), pgx.ErrNoRows)
	require.NoError(t, lwRecv(t, finished))
	var after int64
	var requeued time.Time
	require.NoError(t, p.QueryRow(testContext, `SELECT source_revision,enqueued_at FROM processor_payload_pending WHERE info_hash=$1`, h).Scan(&after, &requeued))
	require.Equal(t, revision, after, "the re-approval queues the row again at the unchanged revision")
	require.True(t, requeued.After(enqueued), "the lease's drop committed before the apply wrote the row back")
	lease, err := s.leasePayload(testContext)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(h), lease.InfoHash)
	require.Equal(t, revision, lease.SourceRevision)
}

func TestLeaseSourceLockSkipsLockedCandidate(t *testing.T) {
	for _, lock := range lwLocks {
		t.Run(lock.name, func(t *testing.T) {
			s, p := lwSetup(t)
			rows := lwRows(t, s, p, 1, 2, 1)
			holder := lwHold(t, p)
			lwLock(t, holder, lock.sql, rows[0])
			lease, took, err := lwLease(s)
			require.NoError(t, err)
			require.Equal(t, hex.EncodeToString(rows[1]), lease.InfoHash, "the locked candidate is passed over")
			require.Less(t, took, lwPrompt)
			rollback(holder)
			lease, err = s.leasePayload(testContext)
			require.NoError(t, err)
			require.Equal(t, hex.EncodeToString(rows[0]), lease.InfoHash, "the skipped row stays queued")
		})
	}
}

func TestLeaseSourceLockAllCandidatesLocked(t *testing.T) {
	s, p := lwSetup(t)
	rows := lwRows(t, s, p, 1, payloadLeaseAttempts, 1)
	holder := lwHold(t, p)
	for i, h := range rows {
		lwLock(t, holder, lwLocks[i%len(lwLocks)].sql, h)
	}
	_, took, err := lwLease(s)
	require.ErrorIs(t, err, pgx.ErrNoRows)
	require.Less(t, took, lwPrompt)
	require.Equal(t, []bool{true, true, true, true}, lwPending(t, p, rows...))
	leases, work := lwWritten(t, p)
	require.Zero(t, leases)
	require.Zero(t, work)
}

func TestLeaseSourceLockCancelFailsTheCall(t *testing.T) {
	s, p := lwSetup(t)
	rows := lwRows(t, s, p, 1, 2, 1)
	lwOldModel(t, p, rows[0])
	ctx, cancel := context.WithCancel(testContext)
	defer cancel()
	// The caller goes away as the lease locks its first candidate's torrent.
	leaser := lwTracedStore(t, s, p, func(_ *pgx.Conn, sql string) {
		if strings.HasPrefix(sql, "SELECT info_hash_v2 FROM torrents WHERE info_hash=$1 FOR UPDATE") {
			cancel()
		}
	})
	_, err := leaser.leasePayload(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, pgx.ErrNoRows, "a cancel fails the call; it is not a 204")
	require.Equal(t, []bool{true, true}, lwPending(t, p, rows...), "the head's drop rolls back with the call")
	leases, work := lwWritten(t, p)
	require.Zero(t, leases)
	require.Zero(t, work)
}

// A test trigger fails the first eligible candidate's source read on the
// server while the context stays live. Only SQLSTATE 55P03 may skip a
// candidate: a cancel or a deadlock fails the call although the next
// candidate could lease.
func TestLeaseSourceLockOtherServerErrorsFailTheCall(t *testing.T) {
	for _, code := range []string{"57014", "40P01"} { // query_canceled, deadlock_detected
		t.Run(code, func(t *testing.T) {
			s, p := lwSetup(t)
			rows := lwRows(t, s, p, 1, 3, 1)
			lwOldModel(t, p, rows[0])
			_, err := p.Exec(testContext, fmt.Sprintf(`CREATE FUNCTION lw_fail() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'synthetic source read failure' USING ERRCODE=TG_ARGV[0]; END$$;
 CREATE TRIGGER lw_fail BEFORE INSERT ON processor_publication_sources FOR EACH ROW WHEN (NEW.info_hash='\x%s'::bytea) EXECUTE FUNCTION lw_fail('%s')`, hex.EncodeToString(rows[1]), code))
			require.NoError(t, err)
			_, err = s.leasePayload(testContext)
			var pgErr *pgconn.PgError
			require.ErrorAs(t, err, &pgErr)
			require.Equal(t, code, pgErr.Code)
			require.Equal(t, []bool{true, true, true}, lwPending(t, p, rows...), "the call aborts as a whole")
			leases, work := lwWritten(t, p)
			require.Zero(t, leases)
			require.Zero(t, work)
		})
	}
}
