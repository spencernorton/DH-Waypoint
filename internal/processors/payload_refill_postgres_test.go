package processors

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/require"
)

// rfSetup registers a required policy reviewer and requires payload proofs,
// as the payload lease tests do.
func rfSetup(t *testing.T) (*Store, *pgxpool.Pool, Entry) {
	t.Helper()
	s, p, _ := fixture(t)
	s.cfg.PayloadVerifierToken = namepolicy.SecretToken(strings.Repeat("verifier", 8))
	svc := newService(t, "synthetic-reviewer", []v1.Stage{v1.Policy}, policyResult)
	svc.identity.ArtifactDigest = strings.Repeat("a", 64)
	entry := register(t, s, svc)
	require.NoError(t, s.SetPublicationConfig(serving.Config{RequiredProcessor: svc.identity.ID, RequiredProcessorArtifactDigest: svc.identity.ArtifactDigest, RequiredPayloadVerifierArtifactDigest: strings.Repeat("b", 64)}))
	return s, p, entry
}

// rfApprove records an approval by the required processor that is current
// for its registry row, at each torrent's current source revision.
func rfApprove(t *testing.T, s *Store, p *pgxpool.Pool, hashes ...[]byte) {
	t.Helper()
	_, err := p.Exec(testContext, `INSERT INTO processor_publication_reviews(info_hash,processor_id,source_revision,execution_generation,processor_identity,approved,reason,job_id,policy_contract)
 SELECT src.info_hash,r.id,src.revision,r.execution_generation,r.manifest->'processor',true,'approved',repeat('f',64),$3
 FROM processor_publication_sources src JOIN processor_registry r ON r.id=$2 WHERE src.info_hash=ANY($1::bytea[])`, hashes, s.publication.RequiredProcessor, serving.PublicationReviewPolicy)
	require.NoError(t, err)
}

// rfProofs records a payload review per torrent at its current source
// revision, expiring age ago and then 1 ms apart in argument order.
func rfProofs(t *testing.T, s *Store, p *pgxpool.Pool, approved bool, age string, hashes ...[]byte) {
	t.Helper()
	_, err := p.Exec(testContext, `INSERT INTO processor_payload_reviews(info_hash,source_revision,verifier_artifact,policy,approved,reason,receipt,expires_at)
 SELECT src.info_hash,src.revision,$2,$3,$4,'synthetic','{}',now()-$5::interval+h.i*interval '1 millisecond'
 FROM unnest($1::bytea[]) WITH ORDINALITY h(info_hash,i) JOIN processor_publication_sources src ON src.info_hash=h.info_hash`,
		hashes, s.publication.RequiredPayloadVerifierArtifactDigest, PayloadEvidenceContract, approved, age)
	require.NoError(t, err)
}

// rfFillers adds n expired proofs, 2 h old and 1 ms apart, for hashes with no
// torrent: they fill refill windows but can never be re-queued.
func rfFillers(t *testing.T, s *Store, p *pgxpool.Pool, n int) {
	t.Helper()
	_, err := p.Exec(testContext, `INSERT INTO processor_payload_reviews(info_hash,source_revision,verifier_artifact,policy,approved,reason,receipt,expires_at)
 SELECT decode('ee'||lpad(to_hex(i),38,'0'),'hex'),1,$1,$2,true,'synthetic','{}',now()-interval '2 hours'+i*interval '1 millisecond' FROM generate_series(1,$3::int) i`,
		s.publication.RequiredPayloadVerifierArtifactDigest, PayloadEvidenceContract, n)
	require.NoError(t, err)
}

// rfPending returns the payload outbox as the first hash byte (test hashes
// are protocol.InfoHash{n}) mapped to the row's priority.
func rfPending(t *testing.T, p *pgxpool.Pool) map[byte]int {
	t.Helper()
	rows, err := p.Query(testContext, `SELECT info_hash,priority FROM processor_payload_pending`)
	require.NoError(t, err)
	defer rows.Close()
	out := map[byte]int{}
	for rows.Next() {
		var h []byte
		var priority int
		require.NoError(t, rows.Scan(&h, &priority))
		out[h[0]] = priority
	}
	require.NoError(t, rows.Err())
	return out
}

// rfRefill runs one refill on its own, so a test sees what it re-queues
// before a lease walk in the same call could drop it again.
func rfRefill(t *testing.T, s *Store) {
	t.Helper()
	tx, err := s.begin(testContext)
	require.NoError(t, err)
	defer rollback(tx)
	advance, err := s.refillPayload(testContext, tx)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(testContext))
	advance()
}

func TestRefillRequeuesOnlyCurrentApprovalsWithHistoricalPriority(t *testing.T) {
	s, p, _ := rfSetup(t)
	// Every torrent starts old, with a current approval and an expired
	// rejected proof; change then breaks or adjusts one fact.
	cases := []struct {
		name, change string
		want         int // priority it is re-queued at, -1 when it stays out
	}{
		{"expired approved proof", `UPDATE processor_payload_reviews SET approved=true WHERE info_hash=$1`, 1},
		{"expired rejected proof", ``, 0},
		{"torrent under 24 h old", `UPDATE torrents SET created_at=now() WHERE info_hash=$1`, 1},
		{"approval withdrawn", `UPDATE processor_publication_reviews SET approved=false,reason='policy_not_approved' WHERE info_hash=$1`, -1},
		{"other policy contract", `UPDATE processor_publication_reviews SET policy_contract='synthetic-retired-contract' WHERE info_hash=$1`, -1},
		{"earlier execution generation", `UPDATE processor_publication_reviews SET execution_generation=execution_generation-1 WHERE info_hash=$1`, -1},
		{"old artifact", `UPDATE processor_publication_reviews SET processor_identity=jsonb_set(processor_identity,'{artifact_digest}',to_jsonb(repeat('c',64))) WHERE info_hash=$1`, -1},
		{"other identity, same artifact", `UPDATE processor_publication_reviews SET processor_identity=jsonb_set(processor_identity,'{version}','"0.9.0"') WHERE info_hash=$1`, -1},
		{"approval at an earlier revision than the proof", `UPDATE processor_publication_reviews SET source_revision=source_revision-1 WHERE info_hash=$1`, -1},
		{"source moved past approval and proof", `UPDATE processor_publication_reviews SET source_revision=source_revision-1 WHERE info_hash=$1;UPDATE processor_payload_reviews SET source_revision=source_revision-1 WHERE info_hash=$1`, -1},
		{"approved by another processor only", `UPDATE processor_publication_reviews SET processor_id='synthetic-other' WHERE info_hash=$1`, -1},
		{"no approval", `DELETE FROM processor_publication_reviews WHERE info_hash=$1`, -1},
		{"proof not expired", `UPDATE processor_payload_reviews SET expires_at=now()+interval '1 hour' WHERE info_hash=$1`, -1},
		// The refill would compute priority 1 here; the existing row must stay as it is.
		{"already pending", `UPDATE processor_payload_reviews SET approved=true WHERE info_hash=$1;INSERT INTO processor_payload_pending(info_hash,source_revision,priority) SELECT info_hash,source_revision,0 FROM processor_payload_reviews WHERE info_hash=$1`, 0},
	}
	hashes := make([][]byte, len(cases))
	for i := range cases {
		hashes[i] = seedTorrent(t, p, byte(i+1), "movie")
	}
	_, err := p.Exec(testContext, `UPDATE torrents SET created_at=now()-interval '2 days'`)
	require.NoError(t, err)
	rfApprove(t, s, p, hashes...)
	rfProofs(t, s, p, false, "1 hour", hashes...)
	for i, c := range cases {
		if c.change != "" {
			_, err = p.Exec(testContext, c.change, hashes[i])
			require.NoError(t, err, c.name)
		}
	}
	rfRefill(t, s)
	pending := rfPending(t, p)
	for i, c := range cases {
		priority, queued := pending[byte(i+1)]
		if c.want < 0 {
			require.False(t, queued, c.name)
			continue
		}
		require.True(t, queued, c.name)
		require.Equal(t, c.want, priority, c.name)
	}
	var stray int
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_payload_pending p JOIN processor_payload_reviews r USING(info_hash) WHERE p.source_revision<>r.source_revision`).Scan(&stray))
	require.Zero(t, stray, "a re-queued row carries its proof's source revision")
	// Everything re-queued can lease: the walk leases a priority-1 row and drops nothing.
	lease, err := s.leasePayload(testContext)
	require.NoError(t, err)
	require.Contains(t, []string{hex.EncodeToString(hashes[0]), hex.EncodeToString(hashes[2])}, lease.InfoHash)
	require.Equal(t, pending, rfPending(t, p))
}

func TestRefillRequeuesContiguousBacklogWithinCeilLeaseCalls(t *testing.T) {
	s, p, _ := rfSetup(t)
	const rows = 200
	hashes := make([][]byte, 0, rows)
	for n := 1; n <= rows; n++ {
		hashes = append(hashes, seedTorrent(t, p, byte(n), "movie"))
	}
	rfApprove(t, s, p, hashes...)
	rfProofs(t, s, p, true, "1 hour", hashes...)
	calls := (rows + payloadRefillRequeues - 1) / payloadRefillRequeues
	require.Equal(t, 7, calls)
	for call := 1; call <= calls; call++ {
		_, err := s.leasePayload(testContext)
		require.NoError(t, err, "call %d", call)
		queued := min(call*payloadRefillRequeues, rows)
		require.Len(t, rfPending(t, p), queued, "call %d", call)
		at, after := s.refillCursor.get()
		if queued < rows {
			require.Equal(t, hashes[queued-1], after, "the cursor stops at the 32nd re-queue of call %d", call)
			continue
		}
		require.True(t, at.IsZero(), "the short last window wraps the cursor")
		require.Empty(t, after)
	}
}

func TestRefillWindowWrapsToRowsBehindTheCursor(t *testing.T) {
	s, p, _ := rfSetup(t)
	r := seedTorrent(t, p, 1, "movie")
	rfApprove(t, s, p, r)
	rfProofs(t, s, p, true, "3 hours", r)
	_, err := p.Exec(testContext, `INSERT INTO processor_payload_pending(info_hash,source_revision,priority) SELECT info_hash,revision,1 FROM processor_publication_sources WHERE info_hash=$1`, r)
	require.NoError(t, err)
	// More than one window behind r, so a single walk cannot reach the present.
	rfFillers(t, s, p, payloadRefillWindow+64)

	// Call 1: the refill passes r while it is still pending; then the lease
	// recheck refuses r under an excluding name policy and deletes its row.
	names, err := namepolicy.New(namepolicy.Config{Enabled: true, ExcludedInfoHashes: []string{hex.EncodeToString(r)}})
	require.NoError(t, err)
	s.names = names
	_, err = s.leasePayload(testContext)
	require.ErrorIs(t, err, pgx.ErrNoRows)
	require.Empty(t, rfPending(t, p))
	var wantAt time.Time
	var wantHash []byte
	require.NoError(t, p.QueryRow(testContext, `SELECT expires_at,info_hash FROM processor_payload_reviews ORDER BY expires_at,info_hash OFFSET $1::int LIMIT 1`, payloadRefillWindow-1).Scan(&wantAt, &wantHash))
	at, after := s.refillCursor.get()
	require.Equal(t, wantHash, after, "a full window without a 32nd re-queue moves the cursor to its end")
	require.True(t, wantAt.Equal(at))

	// e's proof becomes due behind the cursor after the walk passed its position.
	e := seedTorrent(t, p, 2, "movie")
	rfApprove(t, s, p, e)
	rfProofs(t, s, p, true, "4 hours", e)
	s.names = nil

	// Call 2 walks on to the present without revisiting either, and wraps.
	_, err = s.leasePayload(testContext)
	require.ErrorIs(t, err, pgx.ErrNoRows)
	require.Empty(t, rfPending(t, p))
	at, after = s.refillCursor.get()
	require.True(t, at.IsZero(), "a short window wraps the cursor")
	require.Empty(t, after)

	// Call 3 starts the next cycle and picks both up.
	lease, err := s.leasePayload(testContext)
	require.NoError(t, err)
	require.Equal(t, map[byte]int{1: 1, 2: 1}, rfPending(t, p))
	require.Contains(t, []string{hex.EncodeToString(r), hex.EncodeToString(e)}, lease.InfoHash)
}

func TestRefillSkippedWhenConsistencyGuardFails(t *testing.T) {
	s, p, entry := rfSetup(t)
	h := seedTorrent(t, p, 1, "movie")
	rfApprove(t, s, p, h)
	rfProofs(t, s, p, true, "3 hours", h)
	// A full window behind h: any refill that runs moves the cursor off zero.
	// Within one transaction a refill followed by a walk that drops the row
	// would leave the same outbox, so the cursor is what shows it was skipped.
	rfFillers(t, s, p, payloadRefillWindow)
	skipped := func(msg string) {
		t.Helper()
		_, err := s.leasePayload(testContext)
		require.ErrorIs(t, err, pgx.ErrNoRows, msg)
		require.Empty(t, rfPending(t, p), msg)
		at, after := s.refillCursor.get()
		require.True(t, at.IsZero() && len(after) == 0, "%s: the refill ran", msg)
	}
	// The refill's own predicate ignores registry state and mode.
	for _, change := range []string{`state='paused'`, `mode='shadow'`} {
		_, err := p.Exec(testContext, `UPDATE processor_registry SET `+change+` WHERE id=$1`, entry.ID)
		require.NoError(t, err)
		skipped(change)
		_, err = p.Exec(testContext, `UPDATE processor_registry SET state='active',mode='apply' WHERE id=$1`, entry.ID)
		require.NoError(t, err)
	}
	configured := s.publication
	other := configured
	other.RequiredProcessorArtifactDigest = strings.Repeat("c", 64)
	require.NoError(t, s.SetPublicationConfig(other))
	skipped("configured artifact differs from the registry")
	require.NoError(t, s.SetPublicationConfig(configured))
	lease, err := s.leasePayload(testContext)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(h), lease.InfoHash, "with the guard holding, the same row is re-queued and leased")
	_, after := s.refillCursor.get()
	require.NotEmpty(t, after)
}

// rfReenrol re-registers the required processor at artifact while a torrent
// is pending under its approval, then follows that row: the lease walk drops
// it, neither the refill nor Scan brings it back, and only the approval from
// the new review job (Finish's apply) re-enrols it.
func rfReenrol(t *testing.T, artifact string) {
	t.Helper()
	s, p, entry := rfSetup(t)
	h := seedTorrent(t, p, 1, "movie")
	n, err := s.Scan(testContext)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	run(t, s)
	require.Contains(t, rfPending(t, p), byte(1), "the approval enrols the torrent")
	// Its earlier proof has expired, which made the historical refill re-queue it.
	rfProofs(t, s, p, true, "1 hour", h)
	next := newService(t, entry.ID, []v1.Stage{v1.Policy}, policyResult)
	next.identity.ArtifactDigest = artifact
	w := next.write()
	w.ExpectedGeneration = entry.Generation
	_, err = s.Register(testContext, entry.ID, w)
	require.NoError(t, err)
	cfg := s.publication
	cfg.RequiredProcessorArtifactDigest = artifact
	require.NoError(t, s.SetPublicationConfig(cfg))

	_, err = s.leasePayload(testContext)
	require.ErrorIs(t, err, pgx.ErrNoRows)
	require.Empty(t, rfPending(t, p), "the lease walk drops the row its approval no longer covers")

	// A control approved under the new registration shows the refill ran
	// over the dropped row's expired proof and left it out.
	c := seedTorrent(t, p, 2, "movie")
	rfApprove(t, s, p, c)
	rfProofs(t, s, p, true, "1 hour", c)
	rfRefill(t, s)
	require.Equal(t, map[byte]int{2: 1}, rfPending(t, p))
	_, err = p.Exec(testContext, `DELETE FROM torrents WHERE info_hash=$1`, c)
	require.NoError(t, err)

	n, err = s.Scan(testContext)
	require.NoError(t, err)
	require.Equal(t, 1, n, "Scan admits a new review job")
	rfRefill(t, s)
	_, err = s.leasePayload(testContext)
	require.ErrorIs(t, err, pgx.ErrNoRows)
	require.Empty(t, rfPending(t, p), "nothing re-enrols the row before its review job completes")

	run(t, s)
	require.Contains(t, rfPending(t, p), byte(1), "the new approval re-enrols the row")
	lease, err := s.leasePayload(testContext)
	require.NoError(t, err)
	require.Equal(t, hex.EncodeToString(h), lease.InfoHash)
}

func TestRefillAndReentryOfDroppedOldModelRow(t *testing.T) {
	rfReenrol(t, strings.Repeat("c", 64))
}

func TestReentryAfterRegisterWaitsForNewReviewJob(t *testing.T) {
	rfReenrol(t, strings.Repeat("a", 64))
}

func TestReentryOfPrunedContentlessRowOnContentsInsert(t *testing.T) {
	s, p, _ := rfSetup(t)
	h := protocol.InfoHash{1}.Bytes()
	outbox := func() (int64, bool) {
		t.Helper()
		var revision int64
		err := p.QueryRow(testContext, `SELECT source_revision FROM processor_publication_pending WHERE info_hash=$1`, h).Scan(&revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false
		}
		require.NoError(t, err)
		return revision, true
	}
	// The torrent's insert trigger enrols it in the publication outbox before it has contents.
	_, err := p.Exec(testContext, `INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at)VALUES($1,'Synthetic.Amber.2024.1080p.WEB-DL.H264-GROUP.mkv',734003200,false,'single',now(),now())`, h)
	require.NoError(t, err)
	pruned, ok := outbox()
	require.True(t, ok)
	n, err := s.Scan(testContext)
	require.NoError(t, err)
	require.Zero(t, n)
	_, ok = outbox()
	require.False(t, ok, "Scan's prune removes the content-less row")

	_, err = p.Exec(testContext, `INSERT INTO torrent_contents(info_hash,content_type,size,created_at,updated_at)VALUES($1,'movie',734003200,now(),now())`, h)
	require.NoError(t, err)
	revision, ok := outbox()
	require.True(t, ok, "the contents trigger re-enrols the torrent")
	require.Greater(t, revision, pruned)
	n, err = s.Scan(testContext)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	var admitted int64
	require.NoError(t, p.QueryRow(testContext, `SELECT publication_revision FROM processor_jobs WHERE info_hash=$1`, h).Scan(&admitted))
	require.Equal(t, revision, admitted)
}

func TestRefillCursorWaitsForTheLeaseCommit(t *testing.T) {
	s, p, _ := rfSetup(t)
	hashes := make([][]byte, 0, 40)
	for n := 1; n <= 40; n++ {
		hashes = append(hashes, seedTorrent(t, p, byte(n), "movie"))
	}
	rfApprove(t, s, p, hashes...)
	rfProofs(t, s, p, true, "1 hour", hashes...)
	// The lease fails after the refill re-queued its 32 rows.
	_, err := p.Exec(testContext, `CREATE FUNCTION rf_fail() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'synthetic lease failure'; END$$;
CREATE TRIGGER rf_fail BEFORE INSERT ON processor_payload_work FOR EACH ROW EXECUTE FUNCTION rf_fail()`)
	require.NoError(t, err)
	_, err = s.leasePayload(testContext)
	require.ErrorContains(t, err, "synthetic lease failure")
	require.Empty(t, rfPending(t, p), "the failed call's re-queues roll back")
	at, after := s.refillCursor.get()
	require.True(t, at.IsZero(), "the cursor stays where the rolled-back walk began")
	require.Empty(t, after)

	_, err = p.Exec(testContext, `DROP TRIGGER rf_fail ON processor_payload_work`)
	require.NoError(t, err)
	_, err = s.leasePayload(testContext)
	require.NoError(t, err)
	require.Len(t, rfPending(t, p), payloadRefillRequeues, "the retry re-queues the same rows")
	_, after = s.refillCursor.get()
	require.Equal(t, hashes[payloadRefillRequeues-1], after)
}
