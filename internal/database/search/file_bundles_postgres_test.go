package search

import (
	"context"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm/clause"
)

// TestTorrentFilesBundleOnlyAndDualNoDuplicates proves the raw (internal)
// TorrentFiles reader sees bundle-only files, that a torrent with identical
// legacy rows and a bundle yields each file exactly once, that the file
// extension criterion finds a bundle-only torrent, and that the
// torrent-content torrent hydrator fills Files from the bundle.
func TestTorrentFilesBundleOnlyAndDualNoDuplicates(t *testing.T) {
	f := newAdultServingPostgresFixture(t)
	s := &search{q: f.q}
	ctx := context.Background()

	mkHash := func(first byte) protocol.InfoHash {
		var h protocol.InfoHash
		for j := range 20 {
			h[j] = first
		}
		return h
	}
	bundleOnly := mkHash(0xD1)
	dual := mkHash(0xD2)
	paths := []string{"Bundle/a.mkv", "Bundle/b.srt"}

	for _, tc := range []struct {
		hash protocol.InfoHash
		name string
	}{
		{bundleOnly, "Synthetic.Bundle.Only.Release"},
		{dual, "Synthetic.Dual.Storage.Release"},
	} {
		_, err := f.pool.Exec(ctx, `INSERT INTO torrents (info_hash, name, files_status) VALUES ($1, $2, 'multi')`, tc.hash.Bytes(), tc.name)
		require.NoError(t, err)
	}
	_, err := f.pool.Exec(ctx, `INSERT INTO torrent_file_bundles (info_hash, indexes, paths, sizes, extensions)
VALUES ($1, $2, $3, $4, $5), ($6, $2, $3, $4, $5)`,
		bundleOnly.Bytes(), []int32{0, 1}, paths, []int64{5000, 200}, []string{"mkv", "srt"}, dual.Bytes())
	require.NoError(t, err)
	// The dual torrent also keeps the identical legacy rows.
	for i, path := range paths {
		ext, size := "mkv", int64(5000)
		if i == 1 {
			ext, size = "srt", 200
		}
		_, err = f.pool.Exec(ctx, `INSERT INTO torrent_files (info_hash, "index", path, extension, size) VALUES ($1, $2, $3, $4, $5)`,
			dual.Bytes(), i, path, ext, size)
		require.NoError(t, err)
	}

	orderByIndex := query.OrderBy(query.OrderByColumn{OrderByColumn: clause.OrderByColumn{
		Column: clause.Column{Table: model.TableNameTorrentFile, Name: "index"},
	}})
	for _, tc := range []struct {
		name string
		hash protocol.InfoHash
	}{
		{"bundle only", bundleOnly},
		{"dual storage", dual},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := s.TorrentFiles(ctx,
				query.Where(TorrentFileInfoHashCriteria(tc.hash)),
				query.WithTotalCount(true), query.WithAggregationBudget(0), orderByIndex)
			require.NoError(t, err)
			require.EqualValues(t, 2, files.TotalCount)
			require.Len(t, files.Items, 2, "each file exactly once")
			require.EqualValues(t, 0, files.Items[0].Index)
			require.Equal(t, "Bundle/a.mkv", files.Items[0].Path)
			require.EqualValues(t, 1, files.Items[1].Index)
			require.Equal(t, "Bundle/b.srt", files.Items[1].Path)
		})
	}

	// The extension criterion must find the bundle-only torrent by its
	// subtitle file, which exists only inside the bundle.
	byExtension, err := s.Torrents(ctx,
		query.Where(TorrentFileExtensionCriteria("srt")),
		query.WithTotalCount(true), query.WithAggregationBudget(0))
	require.NoError(t, err)
	foundHashes := make([]protocol.InfoHash, 0, len(byExtension.Items))
	for _, item := range byExtension.Items {
		foundHashes = append(foundHashes, item.InfoHash)
	}
	require.Contains(t, foundHashes, bundleOnly)

	// The torrent-content hydrator must fill Torrent.Files from the bundle.
	_, err = f.pool.Exec(ctx, `INSERT INTO torrent_contents (id, info_hash) VALUES ($1, $2)`,
		"synthetic-bundle-only-content", bundleOnly.Bytes())
	require.NoError(t, err)
	content, err := s.TorrentContent(ctx,
		query.Where(TorrentContentInfoHashCriteria(bundleOnly)),
		HydrateTorrentContentTorrent(HydrateTorrentContentTorrentWithFiles()))
	require.NoError(t, err)
	require.Len(t, content.Items, 1)
	require.Equal(t, bundleOnly, content.Items[0].Torrent.InfoHash)
	require.Len(t, content.Items[0].Torrent.Files, 2)
	require.Equal(t, "Bundle/a.mkv", content.Items[0].Torrent.Files[0].Path)
	require.Equal(t, "Bundle/b.srt", content.Items[0].Torrent.Files[1].Path)
}
