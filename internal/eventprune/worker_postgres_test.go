package eventprune

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/verdicts"
	migrationssql "github.com/spencernorton/DH-Waypoint/migrations"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const day = 24 * time.Hour

func fixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL not configured")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	_, err = admin.Exec(ctx, `SELECT pg_advisory_lock(830571);CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;CREATE EXTENSION IF NOT EXISTS btree_gin WITH SCHEMA public;SELECT pg_advisory_unlock(830571)`)
	require.NoError(t, err)
	schema := fmt.Sprintf("eventprune_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	db := stdlib.OpenDB(*cfg.ConnConfig)
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		db.Close()
		pool.Close()
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		_ = admin.Close(ctx)
	})
	return pool
}

func newPruner(batch int) *pruner {
	cfg := NewDefaultConfig()
	cfg.BatchSize = batch
	return &pruner{cfg: cfg, logger: zap.NewNop().Sugar()}
}

func strings(t *testing.T, pool *pgxpool.Pool, sql string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), sql)
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return out
}

func TestFeedbackEventsArePrunedByAge(t *testing.T) {
	pool := fixture(t)
	ctx := context.Background()
	now := time.Now()
	// seq 1-3 are past the 30 day default, 4-5 are inside it.
	for i, age := range []time.Duration{45 * day, 40 * day, 31 * day, 29 * day, time.Hour} {
		_, err := pool.Exec(ctx, `INSERT INTO processor_feedback_events(event_id,processor_id,execution_generation,info_hash,tier,ground_truth,delivery_state,created_at)
VALUES(repeat($1,64),'student',1,decode(repeat('01',20),'hex'),'human_gold','{"outcome":"x"}',$2,$3)`,
			fmt.Sprint(i), []string{"pending", "delivered", "pulled", "pending", "pending"}[i], now.Add(-age))
		require.NoError(t, err)
	}
	w := newPruner(2) // two statements are needed to reach the third old row
	w.runCycle(ctx, pool, now)
	require.Equal(t, []string{"4", "5"}, strings(t, pool, `SELECT seq::text FROM processor_feedback_events ORDER BY seq`))

	w.cfg.FeedbackRetention = 0
	w.runCycle(ctx, pool, now.Add(365*day))
	require.Len(t, strings(t, pool, `SELECT seq::text FROM processor_feedback_events`), 2, "a zero retention turns the pruner off")
}

func TestQBStateObservationsKeepNewestAndPrivacy(t *testing.T) {
	pool := fixture(t)
	ctx := context.Background()
	now := time.Now()
	add := func(name string, hash byte, kind, category string, age time.Duration) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO label_evidence(source,source_kind,source_instance,source_object_id,info_hash,category,observed_at,strength)
VALUES('qbittorrent',$1,'alpha',$2,decode(repeat($3,20),'hex'),$4,$5,10)`, kind, name, fmt.Sprintf("%02x", hash), category, now.Add(-age))
		require.NoError(t, err)
	}
	const state = "qb_state_observation"
	// Torrent 1: old history with a fresh observation on top.
	add("1-old-a", 1, state, "movies", 120*day)
	add("1-old-b", 1, state, "movies", 91*day)
	add("1-inside-window", 1, state, "movies", 30*day)
	add("1-newest", 1, state, "movies", time.Hour)
	add("1-category-poll", 1, "poll_categories", "movies", 200*day)
	// Torrent 2: no longer in the client. Its newest row is old and must stay.
	add("2-old", 2, state, "", 120*day)
	add("2-newest", 2, state, "", 100*day)
	// Torrent 3: once private, later moved to a public category.
	add("3-private", 3, state, " Private ", 120*day)
	add("3-public-newest", 3, state, "movies", time.Hour)
	// Torrent 4: always private.
	add("4-private-old", 4, state, "bitgrab", 120*day)
	add("4-private-newest", 4, state, "bitgrab", 100*day)

	w := newPruner(2)
	w.runCycle(ctx, pool, now)
	require.Equal(t, []string{
		"1-category-poll", "1-inside-window", "1-newest",
		"2-newest",
		"3-private", "3-public-newest",
		"4-private-newest",
	}, strings(t, pool, `SELECT source_object_id FROM label_evidence ORDER BY 1`))
}

func TestVerdictEventsKeepTheCurrentTruth(t *testing.T) {
	pool := fixture(t)
	ctx := context.Background()
	now := time.Now()
	store := verdicts.NewStore(lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }))
	hash := func(n byte) []byte { return append(make([]byte, 19), n) }
	record := func(n byte, verdict, mechanism, reason string, age time.Duration) {
		t.Helper()
		require.NoError(t, store.Record(ctx, verdicts.Event{InfoHash: hash(n), Verdict: verdict, Mechanism: mechanism, Reason: reason}))
		_, err := pool.Exec(ctx, `UPDATE torrent_verdict_events SET created_at=$2 WHERE reason=$1`, reason, now.Add(-age))
		require.NoError(t, err)
	}
	const old = 400 * day
	// Events are recorded oldest first, as in a real ledger: ids follow time.
	// Torrent 1: rejected three times long ago. Only the last one is the truth.
	record(1, verdicts.VerdictTombstoned, verdicts.MechanismBlocking, "1-first", old)
	record(1, verdicts.VerdictTombstoned, verdicts.MechanismBlocking, "1-second", old)
	record(1, verdicts.VerdictTombstoned, verdicts.MechanismBlocking, "1-current", old)
	// Torrent 2: an operator decision is audit history even when superseded.
	record(2, verdicts.VerdictRestored, verdicts.MechanismOperator, "2-operator", old)
	// Torrent 3: an old event that a recovery snapshot points at.
	record(3, verdicts.VerdictTombstoned, verdicts.MechanismClassifierDelete, "3-snapshot", old)
	_, err := pool.Exec(ctx, `INSERT INTO catalogue_recovery_snapshots(info_hash,source_digest,contract_version,state,reason,snapshot,payload_bytes,created_at,expires_at,removed_verdict_event_id)
SELECT info_hash,'digest','v1','removed','synthetic','{}',1,now(),now()+interval '1 day',id FROM torrent_verdict_events WHERE reason='3-snapshot'`)
	require.NoError(t, err)
	// Torrent 5: an old machine verdict later replaced.
	record(5, verdicts.VerdictQuarantined, verdicts.MechanismJunkpurge, "5-superseded", old)
	record(5, verdicts.VerdictTombstoned, verdicts.MechanismBlocking, "5-current", old)
	// Torrent 4: superseded, but still inside the retention window.
	record(4, verdicts.VerdictQuarantined, verdicts.MechanismJunkpurge, "4-recent", 30*day)
	record(2, verdicts.VerdictQuarantined, verdicts.MechanismJunkpurge, "2-current", time.Hour)
	record(3, verdicts.VerdictRestored, verdicts.MechanismOperator, "3-current", time.Hour)
	record(4, verdicts.VerdictRestored, verdicts.MechanismOperator, "4-current", time.Hour)

	const rebuilt = `SELECT encode(info_hash,'hex')||' '||verdict||' '||mechanism FROM (
SELECT DISTINCT ON (info_hash) info_hash,verdict,mechanism FROM torrent_verdict_events ORDER BY info_hash,id DESC) latest ORDER BY 1`
	const state = `SELECT encode(info_hash,'hex')||' '||verdict||' '||mechanism FROM torrent_verdict_state ORDER BY 1`
	before := strings(t, pool, state)
	require.Len(t, before, 5)

	w := newPruner(2) // several statements, so the id cursor is exercised
	w.runCycle(ctx, pool, now)
	require.Equal(t, []string{
		"1-current", "2-current", "2-operator", "3-current", "3-snapshot", "4-current", "4-recent", "5-current",
	}, strings(t, pool, `SELECT reason FROM torrent_verdict_events ORDER BY 1`))
	require.Zero(t, w.verdictCursor, "a finished pass starts over next cycle")
	require.Equal(t, before, strings(t, pool, state), "derived state is never touched")
	require.Equal(t, before, strings(t, pool, rebuilt), "state can still be rebuilt from the events")
}
