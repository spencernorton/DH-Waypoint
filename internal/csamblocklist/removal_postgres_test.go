package csamblocklist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencernorton/DH-Waypoint/internal/csamblocklist/csamblocklisttest"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	migrationssql "github.com/spencernorton/DH-Waypoint/migrations"
	"github.com/stretchr/testify/require"
)

// removalPool is a pool on a fresh, fully migrated schema of the disposable
// database, and a live service that uses it.
func removalPool(t *testing.T) (*pgxpool.Pool, *service) {
	t.Helper()
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL required")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("csam_removal_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	_, err = admin.Exec(ctx, `SELECT pg_advisory_lock(830571);CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;CREATE EXTENSION IF NOT EXISTS btree_gin WITH SCHEMA public;SELECT pg_advisory_unlock(830571)`)
	require.NoError(t, err)
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		_ = admin.Close(ctx)
	})
	db := stdlib.OpenDB(*cfg.ConnConfig)
	defer db.Close()
	migration, err := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, err)
	_, err = migration.Up(ctx)
	require.NoError(t, err)
	c := NewDefaultConfig()
	c.Enabled = false // feeds off: the built-in list still applies
	return pool, New(c, nil, NewMetrics(), lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }), nil).(*service)
}

func strings1(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), sql, args...)
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	return out
}

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

// tablesHolding counts, for every table of the schema, the rows that contain
// text, as written or as the bytes of a bytea column.
func tablesHolding(t *testing.T, pool *pgxpool.Pool, text string) map[string]int {
	t.Helper()
	held := map[string]int{}
	for _, table := range strings1(t, pool, `select tablename::text from pg_tables where schemaname=current_schema() order by 1`) {
		if n := count(t, pool, `select count(*) from `+pgx.Identifier{table}.Sanitize()+` r where position($1 in to_jsonb(r)::text)>0 or position(encode(convert_to($1,'UTF8'),'hex') in to_jsonb(r)::text)>0`, text); n > 0 {
			held[table] = n
		}
	}
	return held
}

// A migration that adds a table keyed by info hash has to say what happens to
// it when a banned-keyword match is removed: it follows the torrents row, it
// is cleaned by a removal step, it protects the torrent, or it was read and
// holds no name or path.
func TestRemovalClassifiesEveryHashTable(t *testing.T) {
	pool, _ := removalPool(t)
	groups := map[string][]string{"cascading": cascadingTables, "protecting": protectingTables, "nameless": namelessTables}
	for _, step := range removalSteps {
		groups["cleaned"] = append(groups["cleaned"], step.table)
	}
	all := strings1(t, pool, `select tablename::text from pg_tables where schemaname=current_schema()`)
	group := map[string]string{}
	for name, tables := range groups {
		for _, table := range tables {
			require.Contains(t, all, table, "%s table no longer exists", name)
			require.Empty(t, group[table], "%s is in two groups", table)
			group[table] = name
		}
	}
	for _, table := range strings1(t, pool, `select table_name::text from information_schema.columns where table_schema=current_schema() and column_name='info_hash' order by 1`) {
		require.NotEmpty(t, group[table], "%s has an info_hash column: add it to a removal step or to one of the table groups in removal.go", table)
	}
	follows := strings1(t, pool, `select conrelid::regclass::text from pg_constraint where contype='f' and confdeltype='c' and confrelid='torrents'::regclass order by 1`)
	slices.Sort(follows)
	require.Equal(t, cascadingTables, follows, "tables that follow a deleted torrents row")
}

// After a removal no table holds the name or a file path, for the torrent
// and for every side table that had a copy. Another torrent is untouched.
func TestRemoveStoredLeavesNoNameOrPath(t *testing.T) {
	pool, svc := removalPool(t)
	ctx := context.Background()
	banned, other := protocol.InfoHash{1, 2, 3}, protocol.InfoHash{9, 8, 7}
	name, path := csamblocklisttest.BannedName()+" 7f3a", "Sample Folder 9c1e/"+csamblocklisttest.BannedName()+".mkv"
	otherName, otherPath := "Sample Clean Clip 41bd", "Sample Folder 66e0/Sample Clean File.mkv"
	for _, seed := range removalSeed {
		_, err := pool.Exec(ctx, seed.sql, banned.Bytes(), name, path)
		require.NoError(t, err, seed.table)
		_, err = pool.Exec(ctx, seed.sql, other.Bytes(), otherName, otherPath)
		require.NoError(t, err, seed.table)
	}
	// The stored row is a hybrid the caller knows by its v1 hash only, and
	// two side tables hold a copy under the v2 hash.
	v2 := make([]byte, 32)
	v2[0] = 0x55
	_, err := pool.Exec(ctx, `update torrents set info_hash_v2=$2 where info_hash=$1`, banned.Bytes(), v2)
	require.NoError(t, err)
	for _, seed := range removalSeed {
		if seed.table == "junkpurge_judgments" || seed.table == "torrent_verdict_events" {
			_, err = pool.Exec(ctx, seed.sql, v2, name, path)
			require.NoError(t, err, seed.table)
		}
	}
	// The seed has to reach every table a step cleans, or this test proves
	// nothing about that step.
	holdingName, holdingPath := tablesHolding(t, pool, name), tablesHolding(t, pool, path)
	for _, step := range removalSteps {
		if !slices.Contains([]string{"llm_work_events", "llm_evaluation_capture_admissions"}, step.table) { // rows without a text column
			require.Positive(t, holdingName[step.table]+holdingPath[step.table], "the seed puts no name or path in %s", step.table)
		}
	}
	otherNameBefore, otherPathBefore := tablesHolding(t, pool, otherName), tablesHolding(t, pool, otherPath)

	stored := model.Torrent{InfoHash: banned, Name: name, FilesStatus: model.FilesStatusMulti, Files: []model.TorrentFile{{Path: path}}}
	outcome, err := svc.RemoveStored(ctx, "processor", stored)
	require.NoError(t, err)
	require.Equal(t, OutcomeRemoved, outcome)

	require.Empty(t, tablesHolding(t, pool, name), "tables that still hold the name")
	require.Empty(t, tablesHolding(t, pool, path), "tables that still hold a file path")
	require.Equal(t, otherNameBefore, tablesHolding(t, pool, otherName), "another torrent's rows")
	require.Equal(t, otherPathBefore, tablesHolding(t, pool, otherPath), "another torrent's rows")
	require.Zero(t, count(t, pool, `select count(*) from junkpurge_judgments where info_hash=$1`, v2), "rows under the v2 hash")
	require.Zero(t, count(t, pool, `select count(*) from torrent_verdict_events where info_hash=$1 and (reason<>'' or evidence is not null)`, v2))

	// Rows keyed by the hash are gone, except the blanked ledgers and the
	// tables that hold no name.
	blanked := []string{"catalogue_recovery_snapshots", "torrent_verdict_events"}
	for table, n := range hashRows(t, pool, banned.Bytes()) {
		if !slices.Contains(blanked, table) && !slices.Contains(namelessTables, table) {
			require.Zero(t, n, "rows left in %s", table)
		}
	}
	require.Equal(t, 1, count(t, pool, `select count(*) from llm_evaluation_captures`), "only the other torrent's capture is left")
	require.Equal(t, 1, count(t, pool, `select count(*) from torrent_verdict_state where info_hash=$1 and verdict='tombstoned' and mechanism='csam'`, banned.Bytes()))
	require.Zero(t, count(t, pool, `select count(*) from torrent_verdict_events where info_hash=$1 and (reason<>'' or evidence is not null)`, banned.Bytes()))
	require.Equal(t, 1, count(t, pool, `select count(*) from torrent_verdict_events where info_hash=$1 and verdict='tombstoned' and mechanism='csam'`, banned.Bytes()))

	// Remembered as a double hash only, in this process and in the table.
	require.True(t, svc.IsBlocked(banned))
	require.False(t, svc.IsBlocked(other))
	dh := sha256.Sum256(banned.Bytes())
	require.Equal(t, 1, count(t, pool, `select count(*) from csam_local_observations where double_hash=$1 and source='processor'`, dh[:]))
	require.Empty(t, tablesHolding(t, pool, hex.EncodeToString(banned.Bytes())+"00"), "sanity: the scan finds nothing for text that is nowhere")

	// Running it again, with the torrent row gone, is not an error.
	outcome, err = svc.RemoveStored(ctx, "processor", stored)
	require.NoError(t, err)
	require.Equal(t, OutcomeRemoved, outcome)
	require.Equal(t, 2, count(t, pool, `select count(*) from torrent_verdict_events where info_hash=$1 and mechanism='csam'`, banned.Bytes()))

	// Nothing but a banned-keyword match can be removed this way.
	_, err = svc.RemoveStored(ctx, "processor", model.Torrent{InfoHash: other, Name: otherName, FilesStatus: model.FilesStatusMulti, Files: []model.TorrentFile{{Path: otherPath}}})
	require.ErrorIs(t, err, ErrNotBannedKeyword)
	// The stored row decides, not the caller's copy: a row that was renamed
	// after the caller read it is not removed.
	_, err = svc.RemoveStored(ctx, "processor", model.Torrent{InfoHash: other, Name: name, FilesStatus: model.FilesStatusMulti, Files: []model.TorrentFile{{Path: path}}})
	require.ErrorIs(t, err, ErrNotBannedKeyword)
	require.Equal(t, otherNameBefore, tablesHolding(t, pool, otherName))
	require.False(t, svc.IsBlocked(other))
}

// qBittorrent and the *arr tools know a v2-only torrent by the first 20 bytes
// of its hash. Evidence under that key protects the torrent.
func TestRemoveStoredProtectsV2OnlyTorrentKnownByItsShortHash(t *testing.T) {
	pool, svc := removalPool(t)
	ctx := context.Background()
	v2 := make([]byte, 32)
	v2[0], v2[31] = 0x66, 0x01
	hash, err := protocol.NewInfoHash(v2)
	require.NoError(t, err)
	name := csamblocklisttest.BannedName() + ".mkv"
	_, err = pool.Exec(ctx, `insert into torrents(info_hash,info_hash_v2,name,size,private,files_status,created_at,updated_at) values($1,$1,$2,4096,false,'single',now(),now())`, v2, name)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `insert into label_evidence(source,source_kind,source_instance,source_object_id,info_hash,observed_at,strength) values('qbittorrent','state','fixture','object',$1,now(),1)`, v2[:20])
	require.NoError(t, err)

	outcome, err := svc.RemoveStored(ctx, "processor", model.Torrent{InfoHash: hash, Name: name, FilesStatus: model.FilesStatusSingle})
	require.NoError(t, err)
	require.Equal(t, OutcomeHiddenProtected, outcome)
	require.Equal(t, 1, count(t, pool, `select count(*) from torrents where info_hash=$1`, v2))
	require.Equal(t, 1, count(t, pool, `select count(*) from processor_publication_holds where info_hash=$1 and held and reason='banned_keyword'`, v2))
}

// hashRows counts the rows keyed by hash in every table with an info_hash column.
func hashRows(t *testing.T, pool *pgxpool.Pool, hash []byte) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range strings1(t, pool, `select table_name::text from information_schema.columns where table_schema=current_schema() and column_name='info_hash'`) {
		out[table] = count(t, pool, `select count(*) from `+pgx.Identifier{table}.Sanitize()+` where info_hash=$1`, hash)
	}
	return out
}

// A match on a torrent tied to the operator's own tools is most likely a
// false hit. It is kept, hidden by a publication hold, and not remembered.
func TestRemoveStoredHidesProtectedTorrents(t *testing.T) {
	v2 := make([]byte, 32)
	v2[0] = 0x77
	for label, protect := range map[string]string{
		"private flag":             `update torrents set private=true where info_hash=$1 and $2::bytea is not null`,
		"canonical label":          `insert into torrent_canonical_labels(info_hash,category,resolved_source,resolved_strength,resolved_at) select $1,'movies','radarr',3,now() where $2::bytea is not null`,
		"arr evidence":             `insert into label_evidence(source,source_kind,source_instance,source_object_id,info_hash,observed_at,strength) select 'radarr','grab','fixture','object',$1,now(),3 where $2::bytea is not null`,
		"evidence keyed by v2":     `insert into label_evidence(source,source_kind,source_instance,source_object_id,info_hash,observed_at,strength) select 'qbittorrent','state','fixture','object',$2,now(),1 where $1::bytea is not null`,
		"grab attempt":             `insert into torrent_grab_attempts(info_hash,source,source_instance,release_title,features) select $1,'radarr','fixture','Sample Release','{}' where $2::bytea is not null`,
		"hint with a content id":   `insert into torrent_hints(info_hash,content_type,content_source,content_id,title,created_at,updated_at) select $1,'movie','tmdb','991001','Sample',now(),now() where $2::bytea is not null`,
		"protected tag":            `insert into torrent_tags(info_hash,name,created_at,updated_at) select $1,'manual',now(),now() where $2::bytea is not null`,
		"qualified protected tag":  `insert into torrent_tags(info_hash,name,created_at,updated_at) select $1,'wanted-sample',now(),now() where $2::bytea is not null`,
		"private qB category":      `insert into label_evidence(source,source_kind,source_instance,source_object_id,info_hash,category,observed_at,strength) select 'qbittorrent','state','fixture','object',$1,'private',now(),1 where $2::bytea is not null`,
		"already held by operator": `insert into torrent_tags(info_hash,name,created_at,updated_at) select $1,'reference',now(),now() where $2::bytea is not null`,
	} {
		t.Run(label, func(t *testing.T) {
			pool, svc := removalPool(t)
			ctx := context.Background()
			hash := protocol.InfoHash{4, 5, 6}
			name := csamblocklisttest.BannedName() + ".mkv"
			_, err := pool.Exec(ctx, `insert into content(type,source,id,title,release_year,created_at,updated_at) values('movie','tmdb','991001','Sample',2031,now(),now())`)
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `insert into torrents(info_hash,info_hash_v2,name,size,private,files_status,created_at,updated_at) values($1,$2,$3,4096,false,'single',now(),now())`, hash.Bytes(), v2, name)
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `insert into junkpurge_judgments(info_hash,verdict,confidence,torrent_name) values($1,'junk',0.9,$2)`, hash.Bytes(), name)
			require.NoError(t, err)
			if label == "already held by operator" {
				_, err = pool.Exec(ctx, `insert into processor_publication_holds(info_hash,held,reason,request_id) values($1,true,'operator_review','operator-request')`, hash.Bytes())
				require.NoError(t, err)
			}
			_, err = pool.Exec(ctx, protect, hash.Bytes(), v2)
			require.NoError(t, err)

			stored := model.Torrent{InfoHash: hash, Name: name, FilesStatus: model.FilesStatusSingle}
			outcome, err := svc.RemoveStored(ctx, "processor", stored)
			require.NoError(t, err)
			require.Equal(t, OutcomeHiddenProtected, outcome)

			require.Equal(t, 1, count(t, pool, `select count(*) from torrents where info_hash=$1 and name=$2`, hash.Bytes(), name), "the torrent is kept")
			require.Equal(t, 1, count(t, pool, `select count(*) from junkpurge_judgments where info_hash=$1`, hash.Bytes()), "its side rows are kept")
			require.Zero(t, count(t, pool, `select count(*) from torrent_verdict_state where info_hash=$1`, hash.Bytes()))
			require.Zero(t, count(t, pool, `select count(*) from csam_local_observations`), "a likely false hit is not remembered or exported")
			require.False(t, svc.IsBlocked(hash))
			held := count(t, pool, `select count(*) from processor_publication_holds where info_hash=$1 and held`, hash.Bytes())
			switch label {
			case "private flag", "private qB category":
				// Served nowhere already; the hold API takes no hold on these either.
				require.Zero(t, held)
			case "already held by operator":
				require.Equal(t, 1, count(t, pool, `select count(*) from processor_publication_holds where info_hash=$1 and held and reason='operator_review'`, hash.Bytes()), "an existing hold keeps its reason")
			default:
				require.Equal(t, 1, held, "hidden by a publication hold")
				require.Equal(t, 1, count(t, pool, `select count(*) from processor_publication_holds where info_hash=$1 and reason='banned_keyword' and request_id ~ '^[a-f0-9]{64}$'`, hash.Bytes()))
			}
			// A released hold is taken again the next time the torrent is seen.
			_, err = pool.Exec(ctx, `update processor_publication_holds set held=false where info_hash=$1`, hash.Bytes())
			require.NoError(t, err)
			_, err = svc.RemoveStored(ctx, "processor", stored)
			require.NoError(t, err)
			require.Equal(t, held, count(t, pool, `select count(*) from processor_publication_holds where info_hash=$1 and held`, hash.Bytes()))
		})
	}
}
