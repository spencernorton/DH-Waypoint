package dhtcrawler

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mi "github.com/anacrolix/torrent/metainfo"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencernorton/DH-Waypoint/internal/csamblocklist"
	"github.com/spencernorton/DH-Waypoint/internal/csamblocklist/csamblocklisttest"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo/banning"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo/metainforequester"
	migrationssql "github.com/spencernorton/DH-Waypoint/migrations"
	"github.com/stretchr/testify/require"
)

// filesRequester serves one made-up torrent per wire key and counts fetches.
type filesRequester struct {
	calls    int
	torrents map[protocol.ID]metainfo.ValidatedInfo
}

func (r *filesRequester) Request(_ context.Context, key protocol.ID, _ netip.AddrPort) (metainforequester.Response, error) {
	r.calls++
	v := r.torrents[key]
	h := protocol.InfoHashV1(key)
	v.Identities = protocol.TorrentIdentities{V1: &h}
	return metainforequester.Response{Info: v.Info, Validated: v}, nil
}

// A torrent whose name or file paths match the built-in banned-keyword list
// is dropped before anything is stored, with no feed configured and with
// feeds switched off. Only its double hash is kept, and neither this process
// nor a restarted one fetches it again. The crawler has no processing mode:
// this check runs ahead of the processor, so it holds in raw, basic and full.
func TestBannedKeywordMatchIsNeverStoredAndIsRemembered(t *testing.T) {
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL required")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer admin.Close(ctx)
	schema := fmt.Sprintf("crawl_banned_keyword_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	_, err = admin.Exec(ctx, `SELECT pg_advisory_lock(830571);CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;CREATE EXTENSION IF NOT EXISTS btree_gin WITH SCHEMA public;SELECT pg_advisory_unlock(830571)`)
	require.NoError(t, err)
	defer admin.Exec(ctx, `DROP SCHEMA `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	defer pool.Close()
	db := stdlib.OpenDB(*cfg.ConnConfig)
	defer db.Close()
	migration, err := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, err)
	_, err = migration.Up(ctx)
	require.NoError(t, err)

	banned := csamblocklisttest.BannedName()
	byName, byPath, clean := protocol.ID{21}, protocol.ID{22}, protocol.ID{23}
	requester := &filesRequester{torrents: map[protocol.ID]metainfo.ValidatedInfo{
		byName: {Info: metainfo.Info{Name: banned + ".mkv", Length: 2 << 30}},
		byPath: {
			Info:  metainfo.Info{Name: "Sample Collection", Length: 2 << 30},
			Files: []mi.FileInfo{{Path: []string{"Sample Folder", "readme.txt"}, Length: 1 << 20}, {Path: []string{banned, "clip 01.mp4"}, Length: 2 << 30}},
		},
		clean: {Info: metainfo.Info{Name: "Sample Clean Clip.mkv", Length: 2 << 30}},
	}}

	exportPath := filepath.Join(t.TempDir(), "export.jsonl")
	newManager := func() csamblocklist.Manager {
		c := csamblocklist.NewDefaultConfig()
		c.Enabled = false // feeds off: the built-in list still applies
		c.ExportFilePath = exportPath
		return csamblocklist.New(c, nil, csamblocklist.NewMetrics(),
			lazy.New(func() (*pgxpool.Pool, error) { return pool, nil }), csamblocklist.NewExporter(c, nil, nil))
	}
	mgr := newManager()
	c := crawler{metainfoRequester: requester, csamBlocklist: mgr, banningChecker: banning.New(banning.Params{}).Checker}
	peers := []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:1")}

	// The persist stage stores what this step returns and nothing else, so
	// an error with an empty response is what keeps a match out of the
	// database. A clean torrent comes back with its metadata.
	for _, key := range []protocol.ID{byName, byPath} {
		res, err := c.doRequestMetaInfo(ctx, key, peers)
		require.ErrorIs(t, err, errBannedKeyword)
		require.Zero(t, res)
	}
	res, err := c.doRequestMetaInfo(ctx, clean, peers)
	require.NoError(t, err)
	require.Equal(t, "Sample Clean Clip.mkv", res.Info.Name)
	require.Equal(t, 3, requester.calls)

	// The double hashes are saved; the infohashes are not.
	wantName, wantPath := sha256.Sum256(byName[:]), sha256.Sum256(byPath[:])
	rows, err := pool.Query(ctx, `SELECT double_hash,source FROM csam_local_observations ORDER BY double_hash`)
	require.NoError(t, err)
	saved := map[[32]byte]string{}
	var dh []byte
	var source string
	_, err = pgx.ForEachRow(rows, []any{&dh, &source}, func() error { saved[[32]byte(dh)] = source; return nil })
	require.NoError(t, err)
	require.Equal(t, map[[32]byte]string{wantName: "crawler", wantPath: "crawler"}, saved)
	export, err := os.ReadFile(exportPath)
	require.NoError(t, err)
	require.Contains(t, string(export), fmt.Sprintf("%x", wantName))
	require.NotContains(t, string(export), fmt.Sprintf("%x", byName[:]))

	// This process does not fetch them again: the egress check and triage both reject.
	hashes := []protocol.InfoHash{protocol.InfoHashV1(byName), protocol.InfoHashV1(byPath), protocol.InfoHashV1(clean)}
	_, err = c.doRequestMetaInfo(ctx, byName, peers)
	require.ErrorIs(t, err, errCsamBlocklistBlocked)
	require.Equal(t, 3, requester.calls)
	require.Equal(t, hashes[2:], mgr.Filter(append([]protocol.InfoHash{}, hashes...)))

	// A refresh moves them from the pending set into the filter.
	require.NoError(t, mgr.Refresh(ctx))
	require.Equal(t, 2, mgr.Snapshot().EntryCount)
	require.Equal(t, hashes[2:], mgr.Filter(append([]protocol.InfoHash{}, hashes...)))

	// A match the table already holds is not exported twice. One the filter
	// answers for without the table holding it is saved all the same: the
	// filter can be wrong, and a feed can drop an entry later.
	mgr.Observe(ctx, "crawler", hashes[0])
	_, err = pool.Exec(ctx, `DELETE FROM csam_local_observations WHERE double_hash=$1`, wantPath[:])
	require.NoError(t, err)
	mgr.Observe(ctx, "crawler", hashes[1])
	var rowCount int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM csam_local_observations`).Scan(&rowCount))
	require.Equal(t, 2, rowCount)
	export, err = os.ReadFile(exportPath)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(string(export), fmt.Sprintf("%x", wantName)))

	// Nor does a restarted one, once its startup refresh has read the table.
	restarted := newManager()
	require.False(t, restarted.IsBlocked(hashes[0]))
	require.NoError(t, restarted.Refresh(ctx))
	require.Equal(t, hashes[2:], restarted.Filter(append([]protocol.InfoHash{}, hashes...)))
}
