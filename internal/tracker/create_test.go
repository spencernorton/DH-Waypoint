package tracker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
	"github.com/stretchr/testify/require"
)

func TestCreateTorrentFileSingle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "release.bin")
	require.NoError(t, os.WriteFile(path, []byte("hello private world"), 0o644))

	raw, hash, err := CreateTorrentFile(path, CreateOptions{
		Private:  true,
		Trackers: []string{"https://tracker.example/announce/KEY"},
	})
	require.NoError(t, err)
	gotHash, info, err := ParseTorrentFile(raw)
	require.NoError(t, err)
	require.Equal(t, hash, gotHash)
	require.NotNil(t, info.Private)
	require.True(t, *info.Private)
	require.Equal(t, "release.bin", info.BestName())
	require.EqualValues(t, len("hello private world"), info.TotalLength())
}

func TestCreateTorrentFileDirectory(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "My.Release")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "subs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "film.mkv"), []byte("fake video bytes"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "subs", "en.srt"), []byte("subtitle"), 0o644))

	raw, hash, err := CreateTorrentFile(root, CreateOptions{Private: true, Source: "home"})
	require.NoError(t, err)
	_, err = metainfo.ParseTorrentFileInfo(hash, raw)
	require.NoError(t, err)
	_, info, err := ParseTorrentFile(raw)
	require.NoError(t, err)
	require.Equal(t, "My.Release", info.BestName())
	require.Len(t, info.Files, 2)
	require.Equal(t, "home", info.Source)

	// Creation is deterministic for identical input.
	raw2, hash2, err := CreateTorrentFile(root, CreateOptions{Private: true, Source: "home"})
	require.NoError(t, err)
	require.Equal(t, hash, hash2)
	require.Equal(t, raw, raw2)

	require.Equal(t, int64(256<<10), DefaultPieceLength(1024))
	require.Equal(t, int64(16<<20), DefaultPieceLength(100<<30))
}
