package serving_test

import (
	"context"
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	"github.com/stretchr/testify/require"
)

// TestServingAdultPolicyBundleOnlyTorrent seeds one legacy multi torrent and
// one bundle-only multi torrent whose file paths carry the strong adult
// keyword "siterip", plus one bundle-only clean torrent. With adult
// exclusion on, only the clean torrent may be served, whether its files live
// in torrent_files rows or solely in a torrent_file_bundles row.
func TestServingAdultPolicyBundleOnlyTorrent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, q := newServingPostgresDatabase(t, ctx)

	mkHash := func(first byte) protocol.InfoHash {
		raw := make([]byte, 20)
		raw[0] = first
		return protocol.MustNewInfoHash(raw)
	}
	legacyAdult := mkHash(0xB1)
	bundleAdult := mkHash(0xB2)
	bundleClean := mkHash(0xB3)

	insertTorrent := func(h protocol.InfoHash, name string, size, filesCount int) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO torrents(info_hash,name,size,private,files_status,files_count,created_at,updated_at)
VALUES($1,$2,$3,false,'multi',$4,now(),now())`, h.Bytes(), name, size, filesCount)
		require.NoError(t, err)
	}
	insertLegacyFile := func(h protocol.InfoHash, index int, path string, size int64) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO torrent_files(info_hash,"index",path,size,created_at,updated_at)
VALUES($1,$2,$3,$4,now(),now())`, h.Bytes(), index, path, size)
		require.NoError(t, err)
	}
	insertBundle := func(h protocol.InfoHash, indexes []int32, paths []string, sizes []int64, extensions []string) {
		t.Helper()
		_, err := pool.Exec(ctx, `INSERT INTO torrent_file_bundles(info_hash,indexes,paths,sizes,extensions)
VALUES($1,$2,$3,$4,$5)`, h.Bytes(), indexes, paths, sizes, extensions)
		require.NoError(t, err)
	}

	// (a) Legacy adult: the siterip path lives in torrent_files rows.
	insertTorrent(legacyAdult, "SyntheticLegacyPack2031", 5010, 2)
	insertLegacyFile(legacyAdult, 0, "Pack/movie.siterip.mkv", 5000)
	insertLegacyFile(legacyAdult, 1, "Pack/readme.txt", 10)

	// (b) Bundle-only adult: the same files live only in a bundle row.
	insertTorrent(bundleAdult, "SyntheticBundlePack2031", 5010, 2)
	insertBundle(bundleAdult,
		[]int32{0, 1},
		[]string{"Pack/movie.siterip.mkv", "Pack/readme.txt"},
		[]int64{5000, 10},
		[]string{"mkv", "txt"})

	// (c) Bundle-only clean: one ordinary movie file.
	insertTorrent(bundleClean, "SyntheticCleanPack2031", 5000, 1)
	insertBundle(bundleClean,
		[]int32{0},
		[]string{"Clean/movie.mkv"},
		[]int64{5000},
		[]string{"mkv"})

	policy, err := serving.NewPolicy(serving.Config{ExcludeAdult: true}, []string{"siterip"}, []string{"mkv"})
	require.NoError(t, err)
	consumer, err := search.New(search.Params{Query: lazy.New(func() (*dao.Query, error) { return q, nil }), ServingPolicy: policy}).ServingSearch.Get()
	require.NoError(t, err)

	torrents, err := consumer.Torrents(ctx, query.WithTotalCount(true), query.WithAggregationBudget(0))
	require.NoError(t, err)
	require.EqualValues(t, 1, torrents.TotalCount, "only the bundle-only clean torrent may be served")
	require.Len(t, torrents.Items, 1)
	require.Equal(t, bundleClean, torrents.Items[0].InfoHash)

	files, err := consumer.TorrentFiles(ctx,
		query.Where(search.TorrentFileInfoHashCriteria(bundleClean)),
		query.WithTotalCount(true), query.WithAggregationBudget(0))
	require.NoError(t, err)
	require.EqualValues(t, 1, files.TotalCount)
	require.Len(t, files.Items, 1)
	require.Equal(t, "Clean/movie.mkv", files.Items[0].Path)
}
