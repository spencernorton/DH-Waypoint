package processors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	"github.com/stretchr/testify/require"
)

// sbOldCatchUpSQL is Scan's catch-up query before the cursor bound was
// repeated inside its probes.
const sbOldCatchUpSQL = `SELECT t.info_hash FROM torrents t WHERE t.info_hash>$1 AND EXISTS(SELECT 1 FROM torrent_contents c WHERE c.info_hash=t.info_hash)
 AND NOT EXISTS(SELECT 1 FROM processor_reprocess_requests r WHERE r.info_hash=t.info_hash AND r.state='waiting_core')
 ORDER BY t.info_hash LIMIT $2`

// sbCatchUpSQL is Scan's catch-up query verbatim. It is inline in Scan, so
// the test checks queue.go still contains it.
const sbCatchUpSQL = `SELECT t.info_hash FROM torrents t WHERE t.info_hash>$1 AND EXISTS(SELECT 1 FROM torrent_contents c WHERE c.info_hash=t.info_hash AND c.info_hash>$1)
 AND NOT EXISTS(SELECT 1 FROM processor_reprocess_requests r WHERE r.info_hash=t.info_hash AND r.state='waiting_core' AND r.info_hash>$1)
 ORDER BY t.info_hash LIMIT $2`

const sbAddContents = `INSERT INTO torrent_contents(info_hash,content_type,size,created_at,updated_at)VALUES($1,'movie',734003200,now(),now())`

// sbItem is the synthetic hash the catalogue fixture derives for item i.
func sbItem(i uint32) []byte {
	h := sha256.Sum256(binary.BigEndian.AppendUint32(nil, i))
	return h[:20]
}

// sbBare inserts a torrent without contents. Its trigger enrols it in the
// publication outbox.
func sbBare(t *testing.T, p *pgxpool.Pool, n byte) []byte {
	t.Helper()
	h := protocol.InfoHash{n}.Bytes()
	_, e := p.Exec(testContext, `INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at)VALUES($1,'Synthetic.Amber.2024.1080p.WEB-DL.H264-GROUP.mkv',734003200,false,'single',now(),now())`, h)
	require.NoError(t, e)
	return h
}

// sbOutbox returns h's outbox revision and whether h is enrolled.
func sbOutbox(t *testing.T, p *pgxpool.Pool, h []byte) (int64, bool) {
	t.Helper()
	var rev int64
	e := p.QueryRow(testContext, `SELECT source_revision FROM processor_publication_pending WHERE info_hash=$1`, h).Scan(&rev)
	if errors.Is(e, pgx.ErrNoRows) {
		return 0, false
	}
	require.NoError(t, e)
	return rev, true
}

func sbRevision(t *testing.T, p *pgxpool.Pool, h []byte) int64 {
	t.Helper()
	var rev int64
	require.NoError(t, p.QueryRow(testContext, `SELECT revision FROM processor_publication_sources WHERE info_hash=$1`, h).Scan(&rev))
	return rev
}

// sbWait polls a boolean query until it holds: a barrier on another
// session's observable state, not a fixed sleep.
func sbWait(t *testing.T, p *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		var ok bool
		require.NoError(t, p.QueryRow(testContext, sql, args...).Scan(&ok))
		if ok {
			return
		}
		require.True(t, time.Now().Before(deadline), "barrier never reached: %s", sql)
		time.Sleep(2 * time.Millisecond)
	}
}

// sbSession runs sql in its own transaction on another connection and
// commits. The backend pid comes first, so the caller can wait for it to block.
func sbSession(p *pgxpool.Pool, sql string, args ...any) (<-chan int32, <-chan error) {
	pid, done := make(chan int32, 1), make(chan error, 1)
	go func() {
		done <- func() error {
			tx, err := p.Begin(testContext)
			if err != nil {
				return err
			}
			defer rollback(tx)
			var id int32
			if err = tx.QueryRow(testContext, `SELECT pg_backend_pid()`).Scan(&id); err != nil {
				return err
			}
			pid <- id
			if _, err = tx.Exec(testContext, sql, args...); err != nil {
				return err
			}
			return tx.Commit(testContext)
		}()
	}()
	return pid, done
}

// sbPid returns the session's backend pid, or fails if the session ended
// without sending one.
func sbPid(t *testing.T, pid <-chan int32, done <-chan error) int32 {
	t.Helper()
	select {
	case id := <-pid:
		return id
	case err := <-done:
		require.FailNow(t, "the session ended before it reported its pid", "%v", err)
		return 0
	}
}

// sbSlowDeletes makes each DELETE statement on table sleep first. Statement
// triggers fire even when a DELETE matches no rows. Scan's hold bound is wall
// time, so this trips it deterministically without changing Scan.
func sbSlowDeletes(t *testing.T, p *pgxpool.Pool, table string, seconds float64) {
	t.Helper()
	_, e := p.Exec(testContext, fmt.Sprintf(`CREATE FUNCTION sb_slow_%[1]s() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN PERFORM pg_sleep(%[2]g); RETURN NULL; END$$;
CREATE TRIGGER sb_slow_%[1]s BEFORE DELETE ON %[1]s FOR EACH STATEMENT EXECUTE FUNCTION sb_slow_%[1]s()`, table, seconds))
	require.NoError(t, e)
}

func sbAdmission(s *Store, outcome string) float64 {
	return testutil.ToFloat64(s.metrics.admission.WithLabelValues(outcome))
}

func sbClassifier(t *testing.T, s *Store) {
	t.Helper()
	register(t, s, newService(t, "sb-classifier", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") }))
}

// sbReady files ready reprocess requests against the current pipeline, so
// settleReprocessing keeps them ready.
func sbReady(t *testing.T, s *Store, p *pgxpool.Pool, hashes ...[]byte) {
	t.Helper()
	status, e := s.Status(testContext)
	require.NoError(t, e)
	for _, h := range hashes {
		_, e = p.Exec(testContext, `INSERT INTO processor_reprocess_requests(request_id,request_digest,origin_job_id,info_hash,expected_pipeline_revision,reason,state,core_job_id,origin_export,before_snapshot)
 VALUES(encode(sha256($1::bytea),'hex'),'synthetic','synthetic',$1,$2,'synthetic_review','ready','synthetic','{}','{}')`, h, status.PipelineRevision)
		require.NoError(t, e)
	}
}

// sbCursorInvariant checks that scan_after never passes an unprocessed
// catch-up hash: exactly the hashes at or below it have jobs.
func sbCursorInvariant(t *testing.T, p *pgxpool.Pool, catchUp [][]byte) []byte {
	t.Helper()
	var cursor []byte
	require.NoError(t, p.QueryRow(testContext, `SELECT scan_after FROM processor_control`).Scan(&cursor))
	for _, h := range catchUp {
		var job bool
		require.NoError(t, p.QueryRow(testContext, `SELECT EXISTS(SELECT 1 FROM processor_jobs WHERE info_hash=$1)`, h).Scan(&job))
		require.Equal(t, bytes.Compare(h, cursor) <= 0, job, "hash %x, cursor %x", h, cursor)
	}
	return cursor
}

type sbPlan struct {
	Relation string   `json:"Relation Name"`
	Hit      int64    `json:"Shared Hit Blocks"`
	Read     int64    `json:"Shared Read Blocks"`
	Plans    []sbPlan `json:"Plans"`
}

// sbExplain returns the shared buffers sql touched in total and in its
// processor_reprocess_requests nodes, and the plan for failure messages.
func sbExplain(t *testing.T, p *pgxpool.Pool, sql string, args ...any) (total, reprocess int64, plan string) {
	t.Helper()
	require.NoError(t, p.QueryRow(testContext, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+sql, args...).Scan(&plan))
	var out []struct{ Plan sbPlan }
	require.NoError(t, json.Unmarshal([]byte(plan), &out))
	var walk func(sbPlan)
	walk = func(n sbPlan) {
		if n.Relation == "processor_reprocess_requests" {
			reprocess += n.Hit + n.Read
		}
		for _, c := range n.Plans {
			walk(c)
		}
	}
	walk(out[0].Plan)
	return out[0].Plan.Hit + out[0].Plan.Read, reprocess, plan
}

func TestSbCatchUpMatchesOldQueryAndStaysBounded(t *testing.T) {
	_, p, _ := fixture(t)
	src, e := os.ReadFile("queue.go")
	require.NoError(t, e)
	require.Contains(t, string(src), "`"+sbCatchUpSQL+"`", "the test must explain Scan's exact catch-up text")
	// 220K torrents; every 11th has no contents (200K contents rows); every
	// 7th has a reprocess request, cycling waiting_core, queued and settled.
	// Enrolment triggers are off for the load only: they double its cost and
	// the catch-up reads no outbox. Short chunks keep each transaction brief,
	// because every fixture's CREATE INDEX CONCURRENTLY waits for them.
	for _, sql := range []string{`ALTER TABLE torrents DISABLE TRIGGER USER`, `ALTER TABLE torrent_contents DISABLE TRIGGER USER`} {
		_, e = p.Exec(testContext, sql)
		require.NoError(t, e)
	}
	for lo := 1; lo <= 220000; lo += 20000 {
		for _, sql := range []string{
			`INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at)
 SELECT substring(sha256(int4send(i)) from 1 for 20),'Synthetic.Item.'||i||'.mkv',1000,false,'single',now(),now() FROM generate_series($1::int,$2::int) i`,
			`INSERT INTO torrent_contents(info_hash,content_type,size,created_at,updated_at)
 SELECT substring(sha256(int4send(i)) from 1 for 20),'movie',1000,now(),now() FROM generate_series($1::int,$2::int) i WHERE i%11<>0`,
			`INSERT INTO processor_reprocess_requests(request_id,request_digest,origin_job_id,info_hash,expected_pipeline_revision,reason,state,core_job_id,origin_export,before_snapshot)
 SELECT encode(sha256(int4send(-i)),'hex'),'synthetic','synthetic',substring(sha256(int4send(i)) from 1 for 20),'synthetic','synthetic_review',(ARRAY['waiting_core','queued','settled'])[i/7%3+1],'synthetic','{}','{}'
 FROM generate_series($1::int,$2::int) i WHERE i%7=0`,
		} {
			_, e = p.Exec(testContext, sql, lo, lo+19999)
			require.NoError(t, e)
		}
	}
	// Without statistics both forms plan sequential scans and prove nothing.
	for _, sql := range []string{`ALTER TABLE torrents ENABLE TRIGGER USER`, `ALTER TABLE torrent_contents ENABLE TRIGGER USER`, `ANALYZE torrents`, `ANALYZE torrent_contents`, `ANALYZE processor_reprocess_requests`} {
		_, e = p.Exec(testContext, sql)
		require.NoError(t, e)
	}
	catchUp := func(sql string, cursor []byte, limit int) [][]byte {
		rows, e := p.Query(testContext, sql, cursor, limit)
		require.NoError(t, e)
		out, e := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		require.NoError(t, e)
		return out
	}
	at := func(b byte) []byte { return append([]byte{b}, make([]byte, 19)...) }
	// Wrapped, start, a short prefix, 50%, 90%, an item with contents, one
	// without, one with a waiting_core request, and the end.
	for _, cursor := range [][]byte{{}, at(0x00), {0x7f}, at(0x80), at(0xe6), sbItem(12345), sbItem(11 * 1001), sbItem(7 * 1500), bytes.Repeat([]byte{0xff}, 20)} {
		for _, limit := range []int{32, 5000} {
			require.Equal(t, catchUp(sbOldCatchUpSQL, cursor, limit), catchUp(sbCatchUpSQL, cursor, limit), "cursor %x, limit %d", cursor, limit)
		}
	}
	require.Len(t, catchUp(sbCatchUpSQL, at(0xe6), 32), 32)
	// Locally the bounded text touches 80-150 buffers at any cursor; the
	// ceilings leave ~7x headroom.
	const ceiling, reprocessCeiling = 1000, 500
	for _, b := range []byte{0x00, 0x80, 0xe6} {
		total, reprocess, plan := sbExplain(t, p, sbCatchUpSQL, at(b), 32)
		require.LessOrEqual(t, total, int64(ceiling), "cursor %02x:\n%s", b, plan)
		require.LessOrEqual(t, reprocess, int64(reprocessCeiling), "the reprocess anti-join, cursor %02x:\n%s", b, plan)
	}
	// Control: here the old text walks both indexes from the start of the
	// hash space (~200K buffers), so the ceilings do separate the two forms.
	total, reprocess, plan := sbExplain(t, p, sbOldCatchUpSQL, at(0xe6), 32)
	require.Greater(t, total, int64(10*ceiling), plan)
	require.Greater(t, reprocess, int64(10*reprocessCeiling), plan)
}

func TestSbPruneVisitsCappedWindowAndWraps(t *testing.T) {
	s, p, _ := fixture(t)
	require.Equal(t, 1024, outboxPruneWindow)
	// Items 1..1500 enter the outbox in item order; every 8th has contents.
	for _, sql := range []string{
		`INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at)
 SELECT decode(lpad(to_hex(i),40,'0'),'hex'),'Synthetic.Item.'||i||'.mkv',1000,false,'single',now(),now() FROM generate_series(1,1500) i`,
		`INSERT INTO torrent_contents(info_hash,content_type,size,created_at,updated_at)
 SELECT decode(lpad(to_hex(i),40,'0'),'hex'),'movie',1000,now(),now() FROM generate_series(8,1500,8) i`,
		`UPDATE processor_publication_pending SET enqueued_at=timestamptz '2026-01-01 00:00:00+00'+(get_byte(info_hash,18)*256+get_byte(info_hash,19))*interval '1 millisecond'`,
	} {
		_, e := p.Exec(testContext, sql)
		require.NoError(t, e)
	}
	item := func(i int) []byte { return binary.BigEndian.AppendUint32(make([]byte, 16), uint32(i)) }
	enrolled := func(lo, hi int, contents bool) int {
		t.Helper()
		var n int
		require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_publication_pending p WHERE p.info_hash BETWEEN $1 AND $2
 AND EXISTS(SELECT 1 FROM torrent_contents c WHERE c.info_hash=p.info_hash)=$3`, item(lo), item(hi), contents).Scan(&n))
		return n
	}
	s.pruneOutbox(testContext)
	require.Zero(t, enrolled(1, 1024, false), "the window's content-less rows are pruned")
	require.Equal(t, 128, enrolled(1, 1024, true), "rows with contents stay")
	require.Equal(t, 476-59, enrolled(1025, 1500, false), "content-less rows past the 1,024-row window are not visited")
	at, after := s.outboxCursor.get()
	require.Equal(t, item(1024), after)
	require.True(t, at.Equal(time.Date(2026, 1, 1, 0, 0, 1, int(24*time.Millisecond), time.UTC)), at)
	// Item 8 loses its contents behind the cursor: its trigger bumps the
	// revision but keeps its place in the outbox order.
	_, e := p.Exec(testContext, `DELETE FROM torrent_contents WHERE info_hash=$1`, item(8))
	require.NoError(t, e)
	s.pruneOutbox(testContext)
	require.Zero(t, enrolled(1025, 1500, false))
	require.Equal(t, 59, enrolled(1025, 1500, true))
	require.Equal(t, 1, enrolled(1, 1024, false), "item 8 waits for the next cycle")
	at, after = s.outboxCursor.get()
	require.True(t, at.IsZero(), "a short window wraps the cursor")
	require.Empty(t, after)
	s.pruneOutbox(testContext)
	require.Zero(t, enrolled(1, 1500, false), "the wrapped walk reaches item 8")
	require.Equal(t, 127+59, enrolled(1, 1500, true))
}

func TestSbPruneRacingContentsInsertKeepsEnrolment(t *testing.T) {
	_, p, _ := fixture(t)
	// window runs the prune's first statement from a zero cursor, as
	// pruneOutbox does, and returns the hashes it saw without contents.
	window := func(tx pgx.Tx) [][]byte {
		t.Helper()
		rows, e := tx.Query(testContext, pruneWindowSQL, time.Time{}, []byte{}, outboxPruneWindow)
		require.NoError(t, e)
		var empty [][]byte
		for rows.Next() {
			var h []byte
			var at time.Time
			var contentless bool
			require.NoError(t, rows.Scan(&h, &at, &contentless))
			if contentless {
				empty = append(empty, h)
			}
		}
		require.NoError(t, rows.Err())
		return empty
	}

	// 1. The contents insert commits after the window read, before the lock.
	x, control := sbBare(t, p, 0x41), sbBare(t, p, 0x42)
	rev := sbRevision(t, p, x)
	tx, e := p.Begin(testContext)
	require.NoError(t, e)
	defer rollback(tx)
	empty := window(tx)
	require.ElementsMatch(t, [][]byte{x, control}, empty)
	_, e = p.Exec(testContext, sbAddContents, x)
	require.NoError(t, e)
	var held [][]byte
	require.NoError(t, queryHashes(testContext, tx, &held, pruneLockSQL, empty))
	require.Len(t, held, 2)
	tag, e := tx.Exec(testContext, pruneDeleteSQL, held)
	require.NoError(t, e)
	require.EqualValues(t, 1, tag.RowsAffected(), "the delete's snapshot sees the committed contents")
	require.NoError(t, tx.Commit(testContext))
	got, ok := sbOutbox(t, p, x)
	require.True(t, ok)
	require.Equal(t, rev+1, got)
	require.Equal(t, rev+1, sbRevision(t, p, x))
	_, ok = sbOutbox(t, p, control)
	require.False(t, ok)

	// 2. The contents insert runs after the lock: its trigger waits on the
	// prune's row lock, then re-enrols the torrent once the delete commits.
	y := sbBare(t, p, 0x43)
	rev = sbRevision(t, p, y)
	tx, e = p.Begin(testContext)
	require.NoError(t, e)
	defer rollback(tx)
	empty = window(tx)
	require.Equal(t, [][]byte{y}, empty)
	held = nil
	require.NoError(t, queryHashes(testContext, tx, &held, pruneLockSQL, empty))
	require.Equal(t, empty, held)
	pid, inserted := sbSession(p, sbAddContents, y)
	sbWait(t, p, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')`, sbPid(t, pid, inserted))
	tag, e = tx.Exec(testContext, pruneDeleteSQL, held)
	require.NoError(t, e)
	require.EqualValues(t, 1, tag.RowsAffected(), "the uncommitted contents are invisible to the delete")
	require.NoError(t, tx.Commit(testContext))
	require.NoError(t, <-inserted)
	got, ok = sbOutbox(t, p, y)
	require.True(t, ok, "the waiting trigger re-enrols the torrent")
	require.Equal(t, rev+1, got)
	require.Equal(t, rev+1, sbRevision(t, p, y))
}

func TestSbPruneSkipsRowsOtherTransactionsHold(t *testing.T) {
	s, p, _ := fixture(t)
	free := [][]byte{sbBare(t, p, 0x51), sbBare(t, p, 0x52)}
	arriving, held := sbBare(t, p, 0x53), sbBare(t, p, 0x54)
	// A contents insert in flight: its trigger has rewritten, and holds,
	// arriving's outbox row.
	insert, e := p.Begin(testContext)
	require.NoError(t, e)
	defer rollback(insert)
	_, e = insert.Exec(testContext, sbAddContents, arriving)
	require.NoError(t, e)
	locker, e := p.Begin(testContext)
	require.NoError(t, e)
	defer rollback(locker)
	_, e = locker.Exec(testContext, `SELECT 1 FROM processor_publication_pending WHERE info_hash=$1 FOR UPDATE`, held)
	require.NoError(t, e)
	start := time.Now()
	s.pruneOutbox(testContext)
	// A wait would end at the prune's 500 ms timeout and roll back, so the
	// committed deletes below prove it skipped; ~10 ms is expected here.
	require.Less(t, time.Since(start), 400*time.Millisecond)
	for _, h := range free {
		_, ok := sbOutbox(t, p, h)
		require.False(t, ok)
	}
	for _, h := range [][]byte{arriving, held} {
		_, ok := sbOutbox(t, p, h)
		require.True(t, ok, "a held row is skipped")
	}
	require.NoError(t, insert.Commit(testContext))
	rollback(locker)
	s.pruneOutbox(testContext) // the short window wrapped, so this revisits both
	rev, ok := sbOutbox(t, p, arriving)
	require.True(t, ok)
	require.Equal(t, sbRevision(t, p, arriving), rev, "the arrived contents keep the torrent enrolled at its new revision")
	_, ok = sbOutbox(t, p, held)
	require.False(t, ok, "the released content-less row is pruned on the next pass")
}

func TestSbPruneNeverLocksRowsWithContents(t *testing.T) {
	s, p, _ := fixture(t)
	withContents := [][]byte{seedTorrent(t, p, 0x61, "movie"), seedTorrent(t, p, 0x62, "movie")}
	empty := [][]byte{sbBare(t, p, 0x63), sbBare(t, p, 0x64)}
	// Park the prune at its DELETE, after its lock statement, on an advisory
	// lock this test holds. Advisory keys are database-wide, hence the run key.
	key := time.Now().UnixNano() & 0x3fffffff
	_, e := p.Exec(testContext, fmt.Sprintf(`CREATE FUNCTION sb_gate() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN PERFORM pg_advisory_xact_lock(%d); RETURN NULL; END$$;
CREATE TRIGGER sb_gate BEFORE DELETE ON processor_publication_pending FOR EACH STATEMENT EXECUTE FUNCTION sb_gate()`, key))
	require.NoError(t, e)
	gate, e := p.Begin(testContext)
	require.NoError(t, e)
	defer rollback(gate)
	_, e = gate.Exec(testContext, `SELECT pg_advisory_xact_lock($1)`, key)
	require.NoError(t, e)
	pruned := make(chan struct{})
	go func() { defer close(pruned); s.pruneOutbox(testContext) }()
	sbWait(t, p, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted AND classid=0 AND objid::bigint=$1)`, key)
	// The prune transaction is open with its locks held; its 500 ms budget
	// covers these probes (a few ms).
	probe := func(h []byte) error {
		tx, e := p.Begin(testContext)
		require.NoError(t, e)
		defer rollback(tx)
		_, e = tx.Exec(testContext, `SELECT 1 FROM processor_publication_pending WHERE info_hash=$1 FOR UPDATE NOWAIT`, h)
		return e
	}
	for _, h := range withContents {
		require.NoError(t, probe(h), "a row with contents is never locked")
	}
	var pgErr *pgconn.PgError
	require.ErrorAs(t, probe(empty[0]), &pgErr, "control: the probe sees the prune's own locks")
	require.Equal(t, "55P03", pgErr.Code)
	rollback(gate)
	<-pruned
	for _, h := range empty {
		_, ok := sbOutbox(t, p, h)
		require.False(t, ok, "the prune committed after the probes")
	}
	for _, h := range withContents {
		_, ok := sbOutbox(t, p, h)
		require.True(t, ok)
	}
}

func TestSbScanPrunesWithFullQueueAndPausedProcessor(t *testing.T) {
	s, p, _ := fixture(t)
	svc := newService(t, "sb-classifier", []v1.Stage{v1.Classify}, func(j v1.Job) v1.Result { return result(j, "movie") })
	entry := register(t, s, svc)
	enrolled := func(h []byte) bool {
		t.Helper()
		_, ok := sbOutbox(t, p, h)
		return ok
	}
	s.cfg.QueueCapacity = 1
	seedTorrent(t, p, 0x71, "movie")
	n, e := s.Scan(testContext)
	require.NoError(t, e)
	require.Equal(t, 1, n, "the queue is now full")
	kept := seedTorrent(t, p, 0x72, "movie")
	pruned := [][]byte{sbBare(t, p, 0x73), sbBare(t, p, 0x74)}
	full := sbAdmission(s, "full")
	n, e = s.Scan(testContext)
	require.NoError(t, e)
	require.Zero(t, n)
	require.Equal(t, full+1, sbAdmission(s, "full"))
	require.True(t, enrolled(kept), "rows with contents stay")
	require.False(t, enrolled(pruned[0]) || enrolled(pruned[1]), "a full queue still prunes")
	paused := "paused"
	require.NoError(t, s.Control(testContext, svc.identity.ID, v1.RegistryControl{ExpectedGeneration: entry.Generation, State: &paused}))
	pruned = [][]byte{sbBare(t, p, 0x75), sbBare(t, p, 0x76)}
	n, e = s.Scan(testContext)
	require.NoError(t, e)
	require.Zero(t, n)
	require.False(t, s.active.Load(), "the paused processor leaves an empty plan")
	require.True(t, enrolled(kept))
	require.False(t, enrolled(pruned[0]) || enrolled(pruned[1]), "a paused processor still prunes")
}

func TestSbScanBoundAdmitsFirstHashOfEachClass(t *testing.T) {
	s, p, _ := fixture(t)
	s.scanHold = scanHoldBound
	sbClassifier(t, s)
	r1, r2 := seedTorrent(t, p, 0x01, "movie"), seedTorrent(t, p, 0x02, "movie")
	o1, o2 := seedTorrent(t, p, 0x11, "movie"), seedTorrent(t, p, 0x12, "movie")
	c1 := seedTorrent(t, p, 0x21, "movie")
	seedTorrent(t, p, 0x22, "movie")
	sbReady(t, s, p, r1, r2)
	// Only o1 and o2 stay in the outbox, and the cursor sits below the
	// catch-up hashes only, so each hash belongs to exactly one class.
	_, e := p.Exec(testContext, `DELETE FROM processor_publication_pending WHERE info_hash<>ALL($1);UPDATE processor_control SET scan_after=$2`, [][]byte{o1, o2}, []byte{0x20})
	require.NoError(t, e)
	// pruneJobs deletes once per Scan after the control row is locked, so the
	// hold is past the bound before the first hash.
	sbSlowDeletes(t, p, "processor_jobs", 0.2)
	n, e := s.Scan(testContext)
	require.NoError(t, e)
	require.Equal(t, 3, n, "the first hash of each class is admitted")
	require.Equal(t, 1.0, sbAdmission(s, "bounded"))
	var jobs [][]byte
	require.NoError(t, p.QueryRow(testContext, `SELECT array_agg(info_hash ORDER BY info_hash) FROM processor_jobs`).Scan(&jobs))
	require.Equal(t, [][]byte{r1, o1, c1}, jobs)
	var states []string
	require.NoError(t, p.QueryRow(testContext, `SELECT array_agg(state ORDER BY info_hash) FROM processor_reprocess_requests`).Scan(&states))
	require.Equal(t, []string{"queued", "ready"}, states)
	_, ok := sbOutbox(t, p, o2)
	require.True(t, ok, "the second outbox hash waits for the next tick")
	var cursor []byte
	require.NoError(t, p.QueryRow(testContext, `SELECT scan_after FROM processor_control`).Scan(&cursor))
	require.Equal(t, c1, cursor, "the cursor stops at the last processed hash")
}

func TestSbScanBoundReadyRequestsCannotStarveOtherClasses(t *testing.T) {
	s, p, _ := fixture(t)
	s.scanHold = scanHoldBound
	sbClassifier(t, s)
	var requests, outbox, catchUp [][]byte
	for n := byte(0x01); n <= 0x10; n++ {
		requests = append(requests, seedTorrent(t, p, n, "movie"))
	}
	// The source digest includes the newest request id (source.go), so a fresh
	// ready request is never already current. Process the requests once, then
	// return them to ready: their sources still match their applications.
	sbReady(t, s, p, requests...)
	for admitted := 0; admitted < len(requests); {
		n, e := s.Scan(testContext)
		require.NoError(t, e)
		require.Positive(t, n)
		admitted += n
	}
	for range requests {
		run(t, s)
	}
	n, e := s.Scan(testContext) // settles the requests and the outbox rows the applications re-enrolled
	require.NoError(t, e)
	require.Zero(t, n, "every request hash is already current")
	_, e = p.Exec(testContext, `UPDATE processor_reprocess_requests SET state='ready',state_reason='normal_processing_completed',result_job_id=NULL`)
	require.NoError(t, e)
	for n := byte(0x21); n <= 0x24; n++ {
		outbox = append(outbox, seedTorrent(t, p, n, "movie"))
	}
	for n := byte(0x41); n <= 0x44; n++ {
		catchUp = append(catchUp, seedTorrent(t, p, n, "movie"))
	}
	_, e = p.Exec(testContext, `DELETE FROM processor_publication_pending WHERE info_hash<>ALL($1);UPDATE processor_control SET scan_after=$2`, outbox, []byte{0x30})
	require.NoError(t, e)
	// Each processed hash deletes its outbox row: at 60 ms a hash, the
	// requests pass the 150 ms bound within their first three every tick.
	sbSlowDeletes(t, p, "processor_publication_pending", 0.06)
	for tick := 1; tick <= len(outbox); tick++ {
		bounded := sbAdmission(s, "bounded")
		_, e = s.Scan(testContext)
		require.NoError(t, e)
		require.Equal(t, bounded+1, sbAdmission(s, "bounded"), "tick %d: the requests trip the bound", tick)
		var ready, left int
		require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_reprocess_requests WHERE state='ready'`).Scan(&ready))
		require.Equal(t, 16, ready, "tick %d: the requests stay on the already-current path", tick)
		require.NoError(t, p.QueryRow(testContext, `SELECT count(*) FROM processor_publication_pending WHERE info_hash=ANY($1)`, outbox).Scan(&left))
		require.LessOrEqual(t, left, len(outbox)-tick, "tick %d: the outbox advances", tick)
		cursor := sbCursorInvariant(t, p, catchUp)
		require.GreaterOrEqual(t, bytes.Compare(cursor, catchUp[tick-1]), 0, "tick %d: the catch-up advances", tick)
	}
}

func TestSbScanBoundCursorNeverPassesUnprocessedHash(t *testing.T) {
	s, p, _ := fixture(t)
	s.scanHold = scanHoldBound
	sbClassifier(t, s)
	var catchUp [][]byte
	for n := byte(0x51); n <= 0x58; n++ {
		catchUp = append(catchUp, seedTorrent(t, p, n, "movie"))
	}
	_, e := p.Exec(testContext, `DELETE FROM processor_publication_pending`)
	require.NoError(t, e)
	// At 60 ms a hash, the bound stops each tick inside the catch-up class.
	sbSlowDeletes(t, p, "processor_publication_pending", 0.06)
	for done := 0; done < len(catchUp); {
		bounded := sbAdmission(s, "bounded")
		_, e = s.Scan(testContext)
		require.NoError(t, e)
		cursor := sbCursorInvariant(t, p, catchUp)
		processed := 0
		for _, h := range catchUp {
			if bytes.Compare(h, cursor) <= 0 {
				processed++
			}
		}
		require.Greater(t, processed, done, "every tick processes a hash")
		done = processed
		if done < len(catchUp) {
			require.Equal(t, bounded+1, sbAdmission(s, "bounded"), "the bound, not the batch, ended the tick")
		}
	}
}

// The prune commits on its own before Scan waits for the control row, so it
// still runs while another session holds that row.
func TestSbPruneRunsBeforeScanTakesControlRow(t *testing.T) {
	s, p, _ := fixture(t)
	bare := sbBare(t, p, 0x81)
	holder := lwHold(t, p)
	_, e := holder.Exec(testContext, `SELECT 1 FROM processor_control WHERE singleton FOR UPDATE`)
	require.NoError(t, e)
	ctx, cancel := context.WithTimeout(testContext, 300*time.Millisecond)
	defer cancel()
	_, e = s.Scan(ctx)
	require.Error(t, e, "Scan cannot take the held control row")
	_, ok := sbOutbox(t, p, bare)
	require.False(t, ok, "the prune ran before Scan waited for the control row")
}

// A ready request whose source leaves no applicable step (here a canonical
// media id with no Policy-capable processor) settles as core_only, as a waiting
// one does with no active plan. Left ready, it would head the request class on every
// tick, and the bound would stop the class after it.
func TestSbScanBoundSettlesRequestsWithNoApplicablePlan(t *testing.T) {
	s, p, _ := fixture(t)
	s.scanHold = scanHoldBound
	sbClassifier(t, s)
	var requests [][]byte
	for n := byte(0x01); n <= 0x04; n++ {
		requests = append(requests, seedTorrent(t, p, n, "movie"))
	}
	_, e := p.Exec(testContext, `INSERT INTO torrent_canonical_labels(info_hash,media_type,media_id,resolved_source,resolved_strength,resolved_at) VALUES($1,'movie','tmdb:990001','synthetic',60,now())`, requests[0])
	require.NoError(t, e)
	sbReady(t, s, p, requests...)
	_, e = p.Exec(testContext, `UPDATE processor_reprocess_requests SET created_at=created_at-interval '10 minutes' WHERE info_hash=$1`, requests[0])
	require.NoError(t, e)
	_, e = p.Exec(testContext, `DELETE FROM processor_publication_pending`)
	require.NoError(t, e)
	_, e = p.Exec(testContext, `UPDATE processor_control SET scan_after=$1`, []byte{0xff})
	require.NoError(t, e)
	// Every tick passes the bound before its second hash.
	sbSlowDeletes(t, p, "processor_jobs", 0.2)
	states := func() []string {
		var out []string
		require.NoError(t, p.QueryRow(testContext, `SELECT array_agg(state ORDER BY created_at) FROM processor_reprocess_requests`).Scan(&out))
		return out
	}

	n, e := s.Scan(testContext)
	require.NoError(t, e)
	require.Zero(t, n)
	require.Equal(t, []string{"core_only", "ready", "ready", "ready"}, states(), "the head settles; the bound stops the rest")
	var reason string
	require.NoError(t, p.QueryRow(testContext, `SELECT state_reason FROM processor_reprocess_requests WHERE info_hash=$1`, requests[0]).Scan(&reason))
	require.Equal(t, "no_applicable_processors", reason)
	for tick := 2; tick <= 4; tick++ {
		n, e = s.Scan(testContext)
		require.NoError(t, e)
		require.Equal(t, 1, n, "tick %d queues the next request", tick)
	}
	require.Equal(t, []string{"core_only", "queued", "queued", "queued"}, states())
	require.Equal(t, 3.0, sbAdmission(s, "bounded"), "ticks with more than one request were bounded")
}

// A ready request whose torrent was deleted is held, as a waiting one is,
// instead of failing every Scan tick until the request expires.
func TestSbScanHoldsReadyRequestWhoseTorrentIsGone(t *testing.T) {
	s, p, _ := fixture(t)
	sbClassifier(t, s)
	gone, kept := seedTorrent(t, p, 0x01, "movie"), seedTorrent(t, p, 0x02, "movie")
	sbReady(t, s, p, gone, kept)
	_, e := p.Exec(testContext, `DELETE FROM torrents WHERE info_hash=$1`, gone)
	require.NoError(t, e)
	n, e := s.Scan(testContext)
	require.NoError(t, e)
	require.Equal(t, 1, n)
	read := func(h []byte) (state, reason string) {
		require.NoError(t, p.QueryRow(testContext, `SELECT state,state_reason FROM processor_reprocess_requests WHERE info_hash=$1`, h).Scan(&state, &reason))
		return
	}
	state, reason := read(gone)
	require.Equal(t, []string{"held", "source_unavailable"}, []string{state, reason})
	state, _ = read(kept)
	require.Equal(t, "queued", state)
}
