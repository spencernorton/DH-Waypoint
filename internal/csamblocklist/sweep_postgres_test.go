package csamblocklist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spencernorton/DH-Waypoint/internal/csamblocklist/csamblocklisttest"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/stretchr/testify/require"
)

// seedTables runs the removal seed for the named tables only.
func seedTables(t *testing.T, pool *pgxpool.Pool, hash byte, name, path string, tables ...string) {
	t.Helper()
	for _, seed := range removalSeed {
		if slices.Contains(tables, seed.table) {
			_, err := pool.Exec(context.Background(), seed.sql, sweepHash(hash), name, path)
			require.NoError(t, err, seed.table)
		}
	}
}

func sweepHash(first byte) []byte {
	h := make([]byte, 20)
	h[0] = first
	return h
}

// A quarantine row in the shape the junk purge wrote: the torrents row and
// its file rows as JSON. The file paths count only with the files status.
func seedQuarantine(t *testing.T, pool *pgxpool.Pool, hash byte, name, path string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `insert into junkpurge_quarantine(info_hash,torrent_name,verdict,confidence,torrent_snapshot,files_snapshot)
 values($1,$2,'junk',0.9,jsonb_build_object('name',$2::text,'files_status','multi'),jsonb_build_array(jsonb_build_object('path',$3::text)))`, sweepHash(hash), name, path)
	require.NoError(t, err)
}

// A recovery snapshot in the shape cataloguerecovery writes: one array of
// rows per table.
func seedRecoverySnapshot(t *testing.T, pool *pgxpool.Pool, hash byte, name, path string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `insert into catalogue_recovery_snapshots(info_hash,source_digest,contract_version,state,reason,snapshot,payload_bytes,created_at,expires_at)
 values($1,encode(sha256($1),'hex'),'seed','removed','seed',
  jsonb_build_object('torrents',jsonb_build_array(jsonb_build_object('name',$2::text,'files_status','multi')),'torrent_files',jsonb_build_array(jsonb_build_object('path',$3::text))),
  10,now(),now()+interval '1 day')`, sweepHash(hash), name, path)
	require.NoError(t, err)
}

// sweepToEnd runs batches of limit rows until the pass is complete. Every
// batch gets a new service, so the position can only come from the database.
func sweepToEnd(t *testing.T, pool *pgxpool.Pool, mode SweepMode, limit int) (*service, sweepState) {
	t.Helper()
	for range 200 {
		svc := New(NewDefaultConfig(), nil, NewMetrics(), lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }), nil).(*service)
		st, err := svc.sweepBatch(context.Background(), mode, limit)
		require.NoError(t, err)
		if st.Done {
			return svc, st
		}
	}
	t.Fatal("the sweep did not finish")
	return nil, sweepState{}
}

// rowCounts is the number of rows in every table but the sweep's own state.
func rowCounts(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range strings1(t, pool, `select tablename::text from pg_tables where schemaname=current_schema() and tablename<>'key_values'`) {
		out[table] = count(t, pool, `select count(*) from `+pgx.Identifier{table}.Sanitize())
	}
	return out
}

var sideSources = []string{
	"junkpurge_quarantine", "junkpurge_judgments", "junkpurge_batch_runs", "junkpurge_batch_attempts",
	"junkpurge_batch_items", "torrent_verdict_events", "eval_frozen",
}

// The operator's path: count first, then remove. The dry run finds every
// stored copy and changes nothing; apply removes each one, hides the ones
// tied to the operator's tools and leaves every clean row alone.
func TestSweepDryRunThenApply(t *testing.T) {
	pool, _ := removalPool(t)
	ctx := context.Background()
	banned := csamblocklisttest.BannedName()
	cleanName, cleanPath := "Sample Clean Clip 41bd", "Sample Folder 66e0/Sample Clean File.mkv"
	// The path of a torrent matched by its name: it goes with the match, so
	// it is not the clean path that is compared afterwards.
	plainPath := "Sample Folder 77aa/Sample Plain File.mkv"

	// Stored torrents: matched by name with a copy in every side table,
	// matched by a file path only, matched but protected, and clean.
	seedTables(t, pool, 0x10, banned+" a1", plainPath, tableNames(removalSeed)...)
	// The same torrent has a v2 hash, and one side row is stored under it.
	// That row belongs to a stored torrent, so it is read and not tested.
	v2 := make([]byte, 32)
	v2[0] = 0xa0
	_, err := pool.Exec(ctx, `update torrents set info_hash_v2=$2 where info_hash=$1`, sweepHash(0x10), v2)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `insert into junkpurge_judgments(info_hash,verdict,confidence,reason,torrent_name) values($1,'junk',0.9,'seed',$2)`, v2, banned+" a1")
	require.NoError(t, err)
	seedTables(t, pool, 0x20, "Sample Clean b2", "Sample Folder b2/"+banned+" b2.mkv", "torrents", "torrent_files")
	seedTables(t, pool, 0x30, banned+" c3", plainPath, "torrents", "junkpurge_judgments")
	_, err = pool.Exec(ctx, `insert into label_evidence(source,source_kind,source_instance,source_object_id,info_hash,observed_at,strength) values('radarr','grab','fixture','stored',$1,now(),3)`, sweepHash(0x30))
	require.NoError(t, err)
	seedTables(t, pool, 0x40, cleanName, cleanPath, tableNames(removalSeed)...)

	// Copies whose torrents row is gone, one hash per table the sweep reads.
	seedQuarantine(t, pool, 0x51, "Sample Clean q1", "Sample Folder q1/"+banned+" q1.mkv")
	seedTables(t, pool, 0x52, banned+" j2", plainPath, "junkpurge_judgments")
	seedTables(t, pool, 0x53, banned+" i3", plainPath, "junkpurge_batch_runs", "junkpurge_batch_attempts", "junkpurge_batch_items")
	seedRecoverySnapshot(t, pool, 0x54, "Sample Clean s4", "Sample Folder s4/"+banned+" s4.mkv")
	// A second item in the same batch run, so the position has to carry the
	// item's place in the run and not only the run.
	_, err = pool.Exec(ctx, `insert into junkpurge_batch_items(run_id,ordinal,custom_id,info_hash,torrent_name,state)
 select r.id,2,'second-item',$2,$3,'pending' from junkpurge_batch_runs r where r.model='seed-'||encode(sha256($1::bytea),'hex')`, sweepHash(0x53), sweepHash(0x59), banned+" i9")
	require.NoError(t, err)
	// Two verdicts for one hash: one removal cleans both.
	seedTables(t, pool, 0x55, banned+" e5", plainPath, "torrent_verdict_events")
	seedTables(t, pool, 0x55, banned+" e5", plainPath, "torrent_verdict_events")
	seedTables(t, pool, 0x56, banned+" f6", plainPath, "eval_frozen")
	// Another segment that sorts first, with a hash that sorts last.
	_, err = pool.Exec(ctx, `insert into eval_frozen(segment,info_hash,name) values('other',$1,$2)`, sweepHash(0x5a), banned+" f0")
	require.NoError(t, err)
	// Gone, matched, and tied to the operator's tools.
	seedTables(t, pool, 0x57, banned+" k7", plainPath, "junkpurge_quarantine")
	_, err = pool.Exec(ctx, `insert into label_evidence(source,source_kind,source_instance,source_object_id,info_hash,observed_at,strength) values('radarr','grab','fixture','gone',$1,now(),3)`, sweepHash(0x57))
	require.NoError(t, err)
	// Gone and clean, in every one of those tables.
	seedTables(t, pool, 0x58, cleanName, cleanPath, sideSources...)
	seedRecoverySnapshot(t, pool, 0x58, cleanName, cleanPath)

	// Every row of a table is read. The stored torrents have side rows too:
	// those are read and not tested.
	scanned := map[string]int64{
		"torrents": 4, "junkpurge_quarantine": 5, "junkpurge_judgments": 6, "junkpurge_batch_items": 5,
		"catalogue_recovery_snapshots": 4, "torrent_verdict_events": 5, "eval_frozen": 5,
	}
	matches := func(remove, hide Outcome, verdicts int64) map[string]map[Outcome]int64 {
		return map[string]map[Outcome]int64{
			"torrents":                     {remove: 2, hide: 1},
			"junkpurge_quarantine":         {remove: 1, hide: 1},
			"junkpurge_judgments":          {remove: 1},
			"junkpurge_batch_items":        {remove: 2},
			"catalogue_recovery_snapshots": {remove: 1},
			"torrent_verdict_events":       {remove: verdicts},
			"eval_frozen":                  {remove: 2},
		}
	}
	for _, src := range sweepSources {
		require.Contains(t, scanned, src.table, "the seed has no row for a table the sweep reads")
		require.True(t, slices.ContainsFunc(removalSteps, func(s struct{ table, sql string }) bool { return s.table == src.table }),
			"%s is read by the sweep but no removal step cleans it", src.table)
	}

	// Dry run, one row a batch: it resumes after every row.
	rowsBefore, heldBefore := rowCounts(t, pool), tablesHolding(t, pool, banned)
	svc, st := sweepToEnd(t, pool, SweepDryRun, 1)
	require.Equal(t, scanned, st.Scanned)
	require.Equal(t, matches(OutcomeWouldRemove, OutcomeWouldHide, 2), st.Matches, "a dry run counts rows")
	require.Equal(t, rowsBefore, rowCounts(t, pool), "a dry run changes no row")
	require.Equal(t, heldBefore, tablesHolding(t, pool, banned), "a dry run blanks nothing")
	require.Equal(t, 1.0, testutil.ToFloat64(svc.metrics.SweepDone))
	require.Equal(t, 2.0, testutil.ToFloat64(svc.metrics.SweepMatches.WithLabelValues("torrents", "would_remove")))
	require.Equal(t, 1.0, testutil.ToFloat64(svc.metrics.SweepMatches.WithLabelValues("torrents", "would_hide_protected")))
	require.Equal(t, 0.0, testutil.ToFloat64(svc.metrics.SweepMatches.WithLabelValues("eval_frozen", "would_hide_protected")), "a zero is published, not left out")
	require.Equal(t, 5.0, testutil.ToFloat64(svc.metrics.SweepRowsScanned.WithLabelValues("junkpurge_quarantine")))

	// Apply is another pass: it starts from the first row again.
	cleanBefore := [2]map[string]int{tablesHolding(t, pool, cleanName), tablesHolding(t, pool, cleanPath)}
	svc, st = sweepToEnd(t, pool, SweepApply, 50)
	// Removing the first stored match deleted its quarantine entry, judgment,
	// batch item and evaluation row before those tables were read. Its
	// snapshot and verdict stay, blanked. The seven removals before the
	// verdict table was read each added one verdict to it.
	require.Equal(t, map[string]int64{
		"torrents": 4, "junkpurge_quarantine": 4, "junkpurge_judgments": 4, "junkpurge_batch_items": 4,
		"catalogue_recovery_snapshots": 4, "torrent_verdict_events": 12, "eval_frozen": 4,
	}, st.Scanned)
	require.Equal(t, matches(OutcomeRemoved, OutcomeHiddenProtected, 1), st.Matches, "one removal for the hash with two verdicts")
	require.Equal(t, 1, count(t, pool, `select count(*) from torrent_verdict_events where info_hash=$1 and mechanism='csam'`, sweepHash(0x55)))
	require.Equal(t, 2.0, testutil.ToFloat64(svc.metrics.SweepMatches.WithLabelValues("torrents", "removed")))
	require.Equal(t, 1.0, testutil.ToFloat64(svc.metrics.SweepDone))

	// Lengths and counts only: a failure must not print a name.
	for _, tag := range []string{" a1", " b2", " q1", " j2", " i3", " i9", " s4", " e5", " f6", " f0"} {
		require.Zero(t, len(tablesHolding(t, pool, banned+tag)), "copies left of match%s", tag)
	}
	require.Equal(t, map[string]int{"torrents": 1, "junkpurge_judgments": 1}, tablesHolding(t, pool, banned+" c3"), "a protected stored match is kept, with its side rows")
	require.Equal(t, 1, count(t, pool, `select count(*) from processor_publication_holds where info_hash=$1 and held and reason='banned_keyword'`, sweepHash(0x30)), "and hidden")
	require.Equal(t, 1, tablesHolding(t, pool, banned+" k7")["junkpurge_quarantine"], "a protected copy without a torrent is kept")
	require.Equal(t, cleanBefore, [2]map[string]int{tablesHolding(t, pool, cleanName), tablesHolding(t, pool, cleanPath)}, "clean rows")
	require.Equal(t, 2, count(t, pool, `select count(*) from torrents`))
	for _, first := range []byte{0x10, 0x20, 0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x59, 0x5a} {
		dh := sha256.Sum256(sweepHash(first))
		require.Equal(t, 1, count(t, pool, `select count(*) from csam_local_observations where double_hash=$1 and source='sweep'`, dh[:]), "match %x is remembered", first)
		require.Equal(t, 1, count(t, pool, `select count(*) from torrent_verdict_state where info_hash=$1 and verdict='tombstoned' and mechanism='csam'`, sweepHash(first)))
	}
	// Ten v1 hashes, and the v2 hash in its full and its 20-byte form.
	require.Equal(t, 12, count(t, pool, `select count(*) from csam_local_observations`), "hidden and clean hashes are not remembered")

	// A finished pass is not run again.
	rowsBefore = rowCounts(t, pool)
	_, again := sweepToEnd(t, pool, SweepApply, 50)
	require.Equal(t, st, again)
	require.Equal(t, rowsBefore, rowCounts(t, pool))
}

func tableNames(seed []struct{ table, sql string }) []string {
	out := make([]string, len(seed))
	for i, s := range seed {
		out[i] = s.table
	}
	return out
}

// A removal that cannot finish stops the sweep at that row. What came before
// it is saved, and the row is tried again: the sweep never skips a match.
func TestSweepWaitsAtARemovalThatFails(t *testing.T) {
	pool, svc := removalPool(t)
	ctx := context.Background()
	name := csamblocklisttest.BannedName() + ".mkv"
	// A saved state that cannot be read starts a new pass.
	_, err := pool.Exec(ctx, `insert into key_values(key,value,created_at,updated_at) values('csam_sweep','not a saved state',now(),now())`)
	require.NoError(t, err)
	seedTables(t, pool, 0x01, name, "unused", "torrents")
	seedTables(t, pool, 0x02, name, "unused", "torrents")

	// Another writer holds the second torrent's row past the removal's 2 s lock wait.
	blocker, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer blocker.Rollback(ctx) //nolint:errcheck
	_, err = blocker.Exec(ctx, `select 1 from torrents where info_hash=$1 for update`, sweepHash(0x02))
	require.NoError(t, err)

	st, err := svc.sweepBatch(ctx, SweepApply, 10)
	require.Error(t, err)
	require.False(t, st.Done)
	require.Equal(t, hex.EncodeToString(sweepHash(0x01)), st.Key, "the position is the last row that was handled")
	require.Equal(t, map[string]int64{"torrents": 1}, st.Scanned)
	require.Equal(t, map[string]map[Outcome]int64{"torrents": {OutcomeRemoved: 1}}, st.Matches)
	require.Equal(t, 1, count(t, pool, `select count(*) from torrents`), "the second match is still stored")
	require.Equal(t, 0.0, testutil.ToFloat64(svc.metrics.SweepDone))

	require.NoError(t, blocker.Rollback(ctx))
	_, st = sweepToEnd(t, pool, SweepApply, 10)
	require.Equal(t, map[string]map[Outcome]int64{"torrents": {OutcomeRemoved: 2}}, st.Matches)
	require.Zero(t, count(t, pool, `select count(*) from torrents`))
}
