package cataloguerecovery_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// TestPostgresRecoveryBundleOnlyRoundtrip converts the standard synthetic
// seed to bundle-only storage (files only in torrent_file_bundles), then
// proves Remove deletes the torrent and its bundle and Restore brings the
// bundle row back with its exact paths — without recreating legacy
// torrent_files rows — and that the effective view still serves both files.
func TestPostgresRecoveryBundleOnlyRoundtrip(t *testing.T) {
	pool := recoveryPool(t)
	h := hash(77)
	seed(t, pool, h, false)

	_, err := pool.Exec(ctx, `INSERT INTO torrent_file_bundles (info_hash, indexes, paths, sizes, extensions)
SELECT info_hash, array_agg("index" ORDER BY "index"), array_agg(path ORDER BY "index"), array_agg(size ORDER BY "index"), array_agg(extension ORDER BY "index")
FROM torrent_files WHERE info_hash=$1 GROUP BY info_hash`, h)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM torrent_files WHERE info_hash=$1`, h)
	require.NoError(t, err)

	countHash := func(table string) int {
		t.Helper()
		var n int
		require.NoError(t, pool.QueryRow(ctx, `select count(*) from `+pgx.Identifier{table}.Sanitize()+` where info_hash=$1`, h).Scan(&n))
		return n
	}
	require.Equal(t, 0, countHash("torrent_files"))
	require.Equal(t, 1, countHash("torrent_file_bundles"))
	require.Equal(t, 2, countHash("torrent_files_effective"))

	s := store(t, pool)
	now := time.Now().UTC()
	snap, err := s.Remove(ctx, h, "blocking", "synthetic bundle roundtrip", now)
	require.NoError(t, err)
	require.Zero(t, countHash("torrents"), "the torrent is removed")
	require.Zero(t, countHash("torrent_file_bundles"), "the bundle follows the removed torrent")

	ok, err := s.Restore(ctx, snap.ID, now.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, countHash("torrents"), "the torrent is restored")

	var restoredPaths []string
	require.NoError(t, pool.QueryRow(ctx, `select paths from torrent_file_bundles where info_hash=$1`, h).Scan(&restoredPaths))
	require.Equal(t, []string{"SyntheticRecovery/Part.1.ENG.mkv", "SyntheticRecovery/Part.2.ENG.mkv"}, restoredPaths)
	require.Zero(t, countHash("torrent_files"), "restore must not recreate legacy file rows")
	require.Equal(t, 2, countHash("torrent_files_effective"))
}
