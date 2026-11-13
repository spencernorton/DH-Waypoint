package processors

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/require"
)

// requireReviewer makes a registered policy reviewer the required
// publication processor, which turns the file-evidence gate on.
func requireReviewer(t *testing.T, s *Store, svc *service) {
	t.Helper()
	require.NoError(t, s.SetPublicationConfig(serving.Config{RequiredProcessor: svc.identity.ID, RequiredProcessorArtifactDigest: svc.identity.ArtifactDigest}))
}

func reviewerFixture(t *testing.T) (*Store, *pgxpool.Pool, *service) {
	t.Helper()
	s, p, _ := fixture(t)
	svc := newService(t, "synthetic-reviewer", []v1.Stage{v1.Policy}, policyResult)
	svc.identity.ArtifactDigest = strings.Repeat("a", 64)
	register(t, s, svc)
	return s, p, svc
}

// seedNamesOnly stores a torrent without its file list, as an import does.
func seedNamesOnly(t *testing.T, p *pgxpool.Pool, n byte) []byte {
	t.Helper()
	h := seedTorrent(t, p, n, "movie")
	_, e := p.Exec(testContext, `UPDATE torrents SET files_status='no_info' WHERE info_hash=$1`, h)
	require.NoError(t, e)
	return h
}

// filesArrive writes the file list the way the crawler does: the torrents row
// first, then its files. Nothing else is touched.
func filesArrive(t *testing.T, p *pgxpool.Pool, h []byte) {
	t.Helper()
	_, e := p.Exec(testContext, `UPDATE torrents SET files_status='multi',files_count=1 WHERE info_hash=$1;
 INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at) VALUES($1,0,'Synthetic.Amber.2024.1080p.WEB-DL.H264-GROUP.mkv',734003200,now(),now())`, h)
	require.NoError(t, e)
}

type reviewState struct {
	Jobs, Reviews, Pending, PayloadPending, PayloadReviews int
	Revision                                               int64
}

func reviewStateOf(t *testing.T, p *pgxpool.Pool, h []byte) reviewState {
	t.Helper()
	var x reviewState
	require.NoError(t, p.QueryRow(testContext, `SELECT
 (SELECT count(*) FROM processor_jobs WHERE info_hash=$1),
 (SELECT count(*) FROM processor_publication_reviews WHERE info_hash=$1),
 (SELECT count(*) FROM processor_publication_pending WHERE info_hash=$1),
 (SELECT count(*) FROM processor_payload_pending WHERE info_hash=$1),
 (SELECT count(*) FROM processor_payload_reviews WHERE info_hash=$1),
 COALESCE((SELECT revision FROM processor_publication_sources WHERE info_hash=$1),0)`, h).
		Scan(&x.Jobs, &x.Reviews, &x.Pending, &x.PayloadPending, &x.PayloadReviews, &x.Revision))
	return x
}

// approvalRows is every row that decides whether a torrent is served.
func approvalRows(t *testing.T, p *pgxpool.Pool, h []byte) string {
	t.Helper()
	var out string
	require.NoError(t, p.QueryRow(testContext, `SELECT jsonb_build_object(
 'revision',(SELECT revision FROM processor_publication_sources WHERE info_hash=$1),
 'reviews',(SELECT jsonb_agg(to_jsonb(r) ORDER BY r.processor_id) FROM processor_publication_reviews r WHERE r.info_hash=$1),
 'payload_reviews',(SELECT jsonb_agg(to_jsonb(r)) FROM processor_payload_reviews r WHERE r.info_hash=$1),
 'payload_pending',(SELECT jsonb_agg(to_jsonb(r)) FROM processor_payload_pending r WHERE r.info_hash=$1),
 'payload_work',(SELECT jsonb_agg(to_jsonb(r)) FROM processor_payload_work r WHERE r.info_hash=$1))::text`, h).Scan(&out))
	return out
}

func scanned(t *testing.T, s *Store) int {
	t.Helper()
	n, e := s.Scan(testContext)
	require.NoError(t, e)
	return n
}

func gated(s *Store) float64 {
	return testutil.ToFloat64(s.metrics.admission.WithLabelValues("file_evidence"))
}

// With a required processor, a torrent that cannot be approved for lack of
// file evidence is not sent for review: no job, no review, no payload row and
// no revision change, and its queue row is gone.
func TestRequiredReviewSkipsTorrentWithoutFileEvidence(t *testing.T) {
	s, p, svc := reviewerFixture(t)
	requireReviewer(t, s, svc)
	namesOnly := seedNamesOnly(t, p, 225)
	// A file list that is only partly stored passes the SQL form of the rule
	// and is refused in Scan by the rule the approval path uses.
	partial := seedTorrent(t, p, 226, "movie")
	_, e := p.Exec(testContext, `UPDATE torrents SET files_status='multi',files_count=2 WHERE info_hash=$1`, partial)
	require.NoError(t, e)
	before := map[string]reviewState{}
	for _, h := range [][]byte{namesOnly, partial} {
		before[string(h)] = reviewStateOf(t, p, h)
		require.Equal(t, 1, before[string(h)].Pending)
	}
	require.Zero(t, scanned(t, s))
	for _, h := range [][]byte{namesOnly, partial} {
		require.Equal(t, reviewState{Revision: before[string(h)].Revision}, reviewStateOf(t, p, h))
	}
	require.Zero(t, svc.calls.Load())
	require.Equal(t, 1.0, gated(s), "the prune removed the names-only row before Scan")
}

// The existing source triggers queue a skipped torrent again when its file
// list arrives, and it is then reviewed.
func TestFileArrivalQueuesSkippedTorrentForReview(t *testing.T) {
	s, p, svc := reviewerFixture(t)
	requireReviewer(t, s, svc)
	h := seedNamesOnly(t, p, 227)
	require.Zero(t, scanned(t, s))
	require.Zero(t, reviewStateOf(t, p, h).Pending)
	filesArrive(t, p, h)
	require.Equal(t, 1, reviewStateOf(t, p, h).Pending)
	require.Equal(t, 1, scanned(t, s))
	run(t, s)
	var approved bool
	var revision int64
	require.NoError(t, p.QueryRow(testContext, `SELECT approved,source_revision FROM processor_publication_reviews WHERE info_hash=$1 AND processor_id=$2`, h, svc.identity.ID).Scan(&approved, &revision))
	require.True(t, approved)
	require.Equal(t, reviewStateOf(t, p, h).Revision, revision)
	require.EqualValues(t, 1, svc.calls.Load())
}

// Without a required processor nothing changes: a torrent without file
// evidence is still queued and reviewed.
func TestNoRequiredProcessorStillReviewsTorrentWithoutFileEvidence(t *testing.T) {
	s, p, svc := reviewerFixture(t)
	h := seedNamesOnly(t, p, 228)
	s.pruneOutbox(testContext)
	require.Equal(t, 1, reviewStateOf(t, p, h).Pending)
	require.Equal(t, 1, scanned(t, s))
	run(t, s)
	var approved bool
	var reason string
	require.NoError(t, p.QueryRow(testContext, `SELECT approved,reason FROM processor_publication_reviews WHERE info_hash=$1`, h).Scan(&approved, &reason))
	require.False(t, approved)
	require.Equal(t, "incomplete_file_evidence", reason)
	require.EqualValues(t, 1, svc.calls.Load())
	// The catch-up pass finds it too.
	other := seedNamesOnly(t, p, 229)
	_, e := p.Exec(testContext, `DELETE FROM processor_publication_pending`)
	require.NoError(t, e)
	require.Equal(t, 1, scanned(t, s))
	require.Equal(t, 1, reviewStateOf(t, p, other).Jobs)
	require.Zero(t, gated(s))
}

// An explicit reprocess request for a torrent without file evidence is held
// with the refusal reason. The hold is final for that request: file arrival
// queues the torrent, not the request.
func TestReprocessRequestWithoutFileEvidenceIsHeld(t *testing.T) {
	s, p, svc := reviewerFixture(t)
	h := seedNamesOnly(t, p, 230)
	// Reviewed once before any processor was required.
	require.Equal(t, 1, scanned(t, s))
	run(t, s)
	page, e := s.Jobs(testContext, 1, "", "")
	require.NoError(t, e)
	origin := page.Items[0].JobID
	requireReviewer(t, s, svc)
	w := reprocessWrite(t, s, origin, "a")
	r, e := s.Reprocess(testContext, origin, w)
	require.NoError(t, e)
	require.Equal(t, "waiting_core", r.State)
	recordReprocessCore(t, s, p, h, w.RequestID, "completed")
	_, e = p.Exec(testContext, `UPDATE queue_jobs SET status='processed',ran_at=clock_timestamp() WHERE id=$1`, r.CoreJobID)
	require.NoError(t, e)
	require.Zero(t, scanned(t, s))
	r, e = s.Reprocessing(testContext, w.RequestID)
	require.NoError(t, e)
	require.Equal(t, []string{"held", "incomplete_file_evidence"}, []string{r.State, r.StateReason})
	require.Nil(t, r.ResultJobID)
	require.Equal(t, 1, reviewStateOf(t, p, h).Jobs)
	require.Equal(t, 1.0, gated(s))
	filesArrive(t, p, h)
	require.Equal(t, 1, scanned(t, s))
	run(t, s)
	r, e = s.Reprocessing(testContext, w.RequestID)
	require.NoError(t, e)
	require.Equal(t, "held", r.State)
	require.Nil(t, r.ResultJobID)
}

// pruneOutbox removes queue rows that fail the file-evidence rule and leaves
// complete ones, and removes nothing without a required processor.
func TestPruneOutboxRemovesRowsWithoutFileEvidence(t *testing.T) {
	s, p, svc := reviewerFixture(t)
	complete := seedTorrent(t, p, 231, "movie")
	namesOnly := seedNamesOnly(t, p, 232)
	sizeless := seedTorrent(t, p, 233, "movie")
	overThreshold := seedTorrent(t, p, 234, "movie")
	_, e := p.Exec(testContext, `UPDATE torrents SET size=0 WHERE info_hash=$1;
 UPDATE torrents SET files_status='over_threshold',files_count=5000 WHERE info_hash=$2`, sizeless, overThreshold)
	require.NoError(t, e)
	queued := func() []int {
		out := []int{}
		for _, h := range [][]byte{complete, namesOnly, sizeless, overThreshold} {
			out = append(out, reviewStateOf(t, p, h).Pending)
		}
		return out
	}
	s.pruneOutbox(testContext)
	require.Equal(t, []int{1, 1, 1, 1}, queued())
	requireReviewer(t, s, svc)
	s.pruneOutbox(testContext)
	require.Equal(t, []int{1, 0, 0, 0}, queued())
}

// The catch-up pass leaves out torrents without file evidence in its query.
func TestCatchUpSkipsTorrentWithoutFileEvidence(t *testing.T) {
	s, p, svc := reviewerFixture(t)
	requireReviewer(t, s, svc)
	complete := seedTorrent(t, p, 235, "movie")
	namesOnly := seedNamesOnly(t, p, 236)
	// Only the catch-up pass can find them.
	_, e := p.Exec(testContext, `DELETE FROM processor_publication_pending`)
	require.NoError(t, e)
	require.Equal(t, 1, scanned(t, s))
	require.Equal(t, 1, reviewStateOf(t, p, complete).Jobs)
	require.Zero(t, reviewStateOf(t, p, namesOnly).Jobs)
	require.Zero(t, gated(s), "left out by the query, not fetched and refused")
}

// The gate deletes a queue row only up to the revision Scan read, so a newer
// source change committed after that read stays queued.
func TestGateKeepsQueueRowOfNewerRevision(t *testing.T) {
	s, p, svc := reviewerFixture(t)
	requireReviewer(t, s, svc)
	// A partial file list passes the prune and is refused in Scan.
	h := seedTorrent(t, p, 250, "movie")
	_, e := p.Exec(testContext, `UPDATE torrents SET files_status='multi',files_count=2 WHERE info_hash=$1;
 UPDATE processor_publication_pending SET source_revision=source_revision+1 WHERE info_hash=$1`, h)
	require.NoError(t, e)
	require.Zero(t, scanned(t, s))
	require.Equal(t, 1.0, gated(s))
	require.Equal(t, 1, reviewStateOf(t, p, h).Pending)
}

// The gated prune re-checks file evidence when it deletes: a file list that
// commits between the prune's window and its delete keeps the row.
func TestGatedPruneKeepsRowWhoseFilesArriveBeforeTheDelete(t *testing.T) {
	_, p, _ := fixture(t)
	x, control := seedNamesOnly(t, p, 251), seedNamesOnly(t, p, 252)
	tx, e := p.Begin(testContext)
	require.NoError(t, e)
	defer rollback(tx)
	rows, e := tx.Query(testContext, pruneWindowSQL, time.Time{}, []byte{}, outboxPruneWindow, true)
	require.NoError(t, e)
	var drop [][]byte
	for rows.Next() {
		var h []byte
		var at time.Time
		var prunable bool
		require.NoError(t, rows.Scan(&h, &at, &prunable))
		if prunable {
			drop = append(drop, h)
		}
	}
	require.NoError(t, rows.Err())
	require.ElementsMatch(t, [][]byte{x, control}, drop)
	_, e = p.Exec(testContext, `UPDATE torrents SET files_status='single' WHERE info_hash=$1`, x)
	require.NoError(t, e)
	var held [][]byte
	require.NoError(t, queryHashes(testContext, tx, &held, pruneLockSQL, drop))
	require.Len(t, held, 2)
	tag, e := tx.Exec(testContext, pruneDeleteSQL, held, true)
	require.NoError(t, e)
	require.EqualValues(t, 1, tag.RowsAffected(), "the delete's snapshot sees the committed file status")
	require.NoError(t, tx.Commit(testContext))
	require.Equal(t, 1, reviewStateOf(t, p, x).Pending)
	require.Zero(t, reviewStateOf(t, p, control).Pending)
}

// A job that turns stale because the core policy changed, as it does at every
// core release, goes back into the queue at the same source revision. Nothing
// that decides serving is touched.
func TestStaleJobReturnsToTheQueueAtTheSameRevision(t *testing.T) {
	s, p, entry := rfSetup(t)
	h := seedTorrent(t, p, 237, "movie")
	require.Equal(t, 1, scanned(t, s))
	run(t, s)
	served := approvalRows(t, p, h)
	state := reviewStateOf(t, p, h)
	require.Equal(t, reviewState{Jobs: 1, Reviews: 1, PayloadPending: 1, Revision: state.Revision}, state)
	// A release changes the core policy, so the catch-up pass reviews again.
	s.policy = "synthetic-core-v2"
	require.Equal(t, 1, scanned(t, s))
	// The next release lands while that job waits.
	s.policy = "synthetic-core-v3"
	_, e := s.RunOne(testContext)
	require.ErrorIs(t, e, ErrStale)
	var stale int
	var queuedAt int64
	require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_jobs WHERE info_hash=$1 AND state='stale'`, h).Scan(&stale))
	require.Equal(t, 1, stale)
	require.NoError(t, p.QueryRow(testContext, `SELECT source_revision FROM processor_publication_pending WHERE info_hash=$1`, h).Scan(&queuedAt))
	require.Equal(t, state.Revision, queuedAt)
	require.Equal(t, served, approvalRows(t, p, h))
	// It is reviewed from the queue, not when the catch-up cursor comes round.
	require.Equal(t, 1, scanned(t, s))
	run(t, s)
	var approved bool
	require.NoError(t, p.QueryRow(testContext, `SELECT approved FROM processor_publication_reviews WHERE info_hash=$1 AND processor_id=$2`, h, entry.ID).Scan(&approved))
	require.True(t, approved)
	require.Equal(t, state.Revision, reviewStateOf(t, p, h).Revision)
}

// The same holds when the policy changes between dispatch and the result.
func TestJobStaleAtFinishReturnsToTheQueue(t *testing.T) {
	s, p, _ := rfSetup(t)
	h := seedTorrent(t, p, 238, "movie")
	require.Equal(t, 1, scanned(t, s))
	revision := reviewStateOf(t, p, h).Revision
	l, e := s.Claim(testContext)
	require.NoError(t, e)
	require.NotNil(t, l)
	j, e := s.Prepare(testContext, *l)
	require.NoError(t, e)
	require.NoError(t, s.startAttempt(testContext, *l))
	s.policy = "synthetic-core-v2"
	require.NoError(t, s.Finish(testContext, *l, j, policyResult(j), true))
	var jobState string
	require.NoError(t, p.QueryRow(testContext, `SELECT state FROM processor_jobs WHERE job_id=$1`, l.ID).Scan(&jobState))
	require.Equal(t, "stale", jobState)
	var queuedAt int64
	require.NoError(t, p.QueryRow(testContext, `SELECT source_revision FROM processor_publication_pending WHERE info_hash=$1`, h).Scan(&queuedAt))
	require.Equal(t, revision, queuedAt)
	require.Equal(t, reviewState{Jobs: 1, Pending: 1, Revision: revision}, reviewStateOf(t, p, h))
}

// A stale job for a torrent without file evidence goes back into the queue
// once, is pruned there, and is never queued again.
func TestRequeuedStaleRowWithoutFileEvidenceIsPrunedOnce(t *testing.T) {
	s, p, svc := reviewerFixture(t)
	h := seedNamesOnly(t, p, 239)
	// Queued before a processor was required.
	require.Equal(t, 1, scanned(t, s))
	requireReviewer(t, s, svc)
	s.policy = "synthetic-core-v2"
	_, e := s.RunOne(testContext)
	require.ErrorIs(t, e, ErrStale)
	revision := reviewStateOf(t, p, h).Revision
	require.Equal(t, reviewState{Jobs: 1, Pending: 1, Revision: revision}, reviewStateOf(t, p, h))
	s.pruneOutbox(testContext)
	require.Equal(t, reviewState{Jobs: 1, Revision: revision}, reviewStateOf(t, p, h))
	for range 3 {
		require.Zero(t, scanned(t, s))
		ran, e := s.RunOne(testContext)
		require.NoError(t, e)
		require.False(t, ran)
	}
	require.Equal(t, reviewState{Jobs: 1, Revision: revision}, reviewStateOf(t, p, h))
	require.Zero(t, svc.calls.Load())
}

// Without a required processor a stale job is not put back, as before.
func TestNoRequiredProcessorLeavesStaleJobOutOfTheQueue(t *testing.T) {
	s, p, _ := reviewerFixture(t)
	atPrepare, atFinish := seedTorrent(t, p, 240, "movie"), seedTorrent(t, p, 241, "movie")
	require.Equal(t, 2, scanned(t, s))
	l, e := s.Claim(testContext)
	require.NoError(t, e)
	require.NotNil(t, l)
	j, e := s.Prepare(testContext, *l)
	require.NoError(t, e)
	require.NoError(t, s.startAttempt(testContext, *l))
	s.policy = "synthetic-core-v2"
	require.NoError(t, s.Finish(testContext, *l, j, policyResult(j), true))
	_, e = s.RunOne(testContext)
	require.ErrorIs(t, e, ErrStale)
	for _, h := range [][]byte{atPrepare, atFinish} {
		var state string
		require.NoError(t, p.QueryRow(testContext, `SELECT state FROM processor_jobs WHERE info_hash=$1`, h).Scan(&state))
		require.Equal(t, "stale", state)
		require.Zero(t, reviewStateOf(t, p, h).Pending)
	}
}

// A torrent stored before the review queue existed, and unchanged since, has
// no processor_publication_sources row and is at revision 1. A job found stale
// at preparation returns it to the queue at revision 1 without writing that
// row: after a core release, and after a change no trigger covers, such as a
// new core receipt.
func TestStaleJobWithoutSourceRowReturnsToTheQueue(t *testing.T) {
	for name, change := range map[string]func(*testing.T, *Store, *pgxpool.Pool, []byte){
		"core release": func(_ *testing.T, s *Store, _ *pgxpool.Pool, _ []byte) { s.policy = "synthetic-core-v2" },
		"new core receipt": func(t *testing.T, s *Store, p *pgxpool.Pool, h []byte) {
			recordReprocessCore(t, s, p, h, "", "completed")
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, p, _ := rfSetup(t)
			h := seedTorrent(t, p, 253, "movie")
			_, e := p.Exec(testContext, `DELETE FROM processor_publication_pending WHERE info_hash=$1;
 DELETE FROM processor_publication_sources WHERE info_hash=$1`, h)
			require.NoError(t, e)
			require.Equal(t, 1, scanned(t, s), "the catch-up pass admits it")
			change(t, s, p, h)
			_, e = s.RunOne(testContext)
			require.ErrorIs(t, e, ErrStale)
			queuedAt, ok := sbOutbox(t, p, h)
			require.True(t, ok)
			require.EqualValues(t, 1, queuedAt)
			require.Equal(t, reviewState{Jobs: 1, Pending: 1}, reviewStateOf(t, p, h), "still no sources row")
			// Only the queue can find it now.
			_, e = p.Exec(testContext, `UPDATE processor_control SET scan_after='\xff'::bytea WHERE singleton`)
			require.NoError(t, e)
			require.Equal(t, 1, scanned(t, s))
			require.Equal(t, reviewState{Jobs: 2}, reviewStateOf(t, p, h))
		})
	}
}

// A stale job for a deleted torrent puts nothing back in the queue, at
// preparation or at the result.
func TestStaleJobForDeletedTorrentLeavesNoQueueRow(t *testing.T) {
	s, p, _ := rfSetup(t)
	a, b := seedTorrent(t, p, 254, "movie"), seedTorrent(t, p, 255, "movie")
	require.Equal(t, 2, scanned(t, s))
	l, e := s.Claim(testContext)
	require.NoError(t, e)
	require.NotNil(t, l)
	j, e := s.Prepare(testContext, *l)
	require.NoError(t, e)
	require.NoError(t, s.startAttempt(testContext, *l))
	_, e = p.Exec(testContext, `DELETE FROM torrents WHERE info_hash=ANY($1)`, [][]byte{a, b})
	require.NoError(t, e)
	require.NoError(t, s.Finish(testContext, *l, j, policyResult(j), true))
	_, e = s.RunOne(testContext)
	require.ErrorIs(t, e, pgx.ErrNoRows)
	for _, h := range [][]byte{a, b} {
		var state string
		require.NoError(t, p.QueryRow(testContext, `SELECT state FROM processor_jobs WHERE info_hash=$1`, h).Scan(&state))
		require.Equal(t, "stale", state)
		require.Zero(t, reviewStateOf(t, p, h).Pending)
	}
}

// The approval path and the gate share one rule. Every file-evidence refusal
// of the approval path is the gate's reason, and what the gate lets through
// is refused only for other reasons.
func TestFileEvidenceReasonIsTheApprovalRule(t *testing.T) {
	one, two, zero := int64(1), int64(2), int64(0)
	for name, tc := range map[string]struct {
		src  source
		want string
	}{
		"single":                     {source{Size: 100, FilesStatus: "single"}, ""},
		"multi":                      {source{Size: 100, FilesStatus: "multi", FileCount: &two, Files: []sourceFile{{Path: "a.mkv", Size: 60}, {Path: "b.nfo", Size: 40}}}, ""},
		"no size":                    {source{FilesStatus: "single"}, "missing_payload_evidence"},
		"no size and no files":       {source{FilesStatus: "no_info"}, "missing_payload_evidence"},
		"no files":                   {source{Size: 100, FilesStatus: "no_info"}, "incomplete_file_evidence"},
		"over the file threshold":    {source{Size: 100, FilesStatus: "over_threshold", FileCount: &two}, "incomplete_file_evidence"},
		"multi without a count":      {source{Size: 100, FilesStatus: "multi", Files: []sourceFile{{Path: "a.mkv", Size: 100}}}, "incomplete_file_evidence"},
		"multi with a zero count":    {source{Size: 100, FilesStatus: "multi", FileCount: &zero}, "incomplete_file_evidence"},
		"multi with a missing file":  {source{Size: 100, FilesStatus: "multi", FileCount: &two, Files: []sourceFile{{Path: "a.mkv", Size: 100}}}, "incomplete_file_evidence"},
		"negative file size":         {source{Size: 100, FilesStatus: "multi", FileCount: &one, Files: []sourceFile{{Path: "a.mkv", Size: -1}}}, "inconsistent_file_evidence"},
		"files larger than torrent":  {source{Size: 100, FilesStatus: "multi", FileCount: &one, Files: []sourceFile{{Path: "a.mkv", Size: 101}}}, "inconsistent_file_evidence"},
		"files smaller than torrent": {source{Size: 100, FilesStatus: "multi", FileCount: &one, Files: []sourceFile{{Path: "a.mkv", Size: 99}}}, "inconsistent_file_evidence"},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, fileEvidenceReason(tc.src))
			// "software" has no further payload rule, so only file evidence can refuse it.
			require.Equal(t, tc.want, publicationPayloadReason(tc.src, "software"))
			if tc.want == "" {
				require.Equal(t, "unresolved_content_type", publicationPayloadReason(tc.src, "unknown"))
			} else {
				require.Equal(t, tc.want, publicationPayloadReason(tc.src, "unknown"))
			}
		})
	}
}
