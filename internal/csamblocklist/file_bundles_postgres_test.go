package csamblocklist

import (
	"bytes"
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/spencernorton/DH-Waypoint/internal/csamblocklist/csamblocklisttest"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/require"
)

// TestBundleOnlyTorrentSweepAndRemoval seeds a clean-named multi torrent
// whose only file path — carrying a banned keyword — lives solely in a
// torrent_file_bundles row. The torrents sweep source must surface that
// path, and RemoveStored must delete both the torrent and its bundle.
func TestBundleOnlyTorrentSweepAndRemoval(t *testing.T) {
	pool, svc := removalPool(t)
	ctx := context.Background()
	banned := csamblocklisttest.BannedName()
	cleanName := "Sample Clean Bundle c1"
	// The same known-matching shape the sweep suite seeds for a
	// path-only match.
	bannedPath := "Sample Folder b2/" + banned + " b2.mkv"
	hash := protocol.MustNewInfoHash(sweepHash(0xC1))
	seedTables(t, pool, 0xC1, cleanName, bannedPath, "torrents", "torrent_file_bundles")

	// No names or paths in failure output: compare lengths and booleans,
	// never print the banned strings themselves.
	type sweepRow struct {
		key    string
		hash   []byte
		v2     []byte
		name   string
		status string
		paths  []string
		test   bool
	}
	found, err := pool.Query(ctx, sweepSources[0].sql, "", 10)
	require.NoError(t, err)
	rows, err := pgx.CollectRows(found, func(r pgx.CollectableRow) (out sweepRow, err error) {
		return out, r.Scan(&out.key, &out.hash, &out.v2, &out.name, &out.status, &out.paths, &out.test)
	})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.True(t, bytes.Equal(hash.Bytes(), rows[0].hash))
	require.True(t, rows[0].test)
	require.Contains(t, rows[0].paths, bannedPath, "the sweep must see the bundle-only file path")

	outcome, err := svc.RemoveStored(ctx, "test", model.Torrent{
		InfoHash: hash, Name: cleanName, FilesStatus: model.FilesStatusMulti,
		Files: []model.TorrentFile{{Path: bannedPath}},
	})
	require.NoError(t, err)
	require.Equal(t, OutcomeRemoved, outcome)
	require.Zero(t, count(t, pool, `select count(*) from torrents where info_hash=$1`, hash.Bytes()))
	require.Zero(t, count(t, pool, `select count(*) from torrent_file_bundles where info_hash=$1`, hash.Bytes()),
		"the bundle follows the deleted torrents row")
}
