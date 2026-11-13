package tracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	mi "github.com/anacrolix/torrent/metainfo"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
	"github.com/stretchr/testify/require"
)

// variedPath returns a path n bytes long once joined with "/". Its text is
// hash output, so it is stored at its full length.
func variedPath(n int) []string {
	var text string
	for sum := sha256.Sum256(nil); len(text) < n; sum = sha256.Sum256(sum[:]) {
		text += base64.RawURLEncoding.EncodeToString(sum[:])
	}
	var parts []string
	for n > 200 {
		parts, text, n = append(parts, text[:200]), text[200:], n-201
	}
	return append(parts, text[:n])
}

// A file path of MaxPathLen bytes is stored whole. A longer one is refused as
// an invalid torrent file and nothing is stored for it.
func TestPostgresUploadPathLengthBounds(t *testing.T) {
	store, pool := trackerFixture(t, NewDefaultConfig())
	ctx := context.Background()
	upload := func(n int) error {
		infoBytes, err := bencode.Marshal(mi.Info{
			Name: fmt.Sprintf("Synthetic.Paths.%d", n), PieceLength: 16384, Pieces: bytes.Repeat([]byte{9}, 20),
			Files: []mi.FileInfo{{Length: 1000, Path: variedPath(n)}},
		})
		require.NoError(t, err)
		meta := mi.MetaInfo{InfoBytes: infoBytes}
		var buf bytes.Buffer
		require.NoError(t, meta.Write(&buf))
		_, err = store.Upload(ctx, buf.Bytes())
		return err
	}

	require.NoError(t, upload(metainfo.MaxPathLen))
	require.ErrorIs(t, upload(metainfo.MaxPathLen+1), ErrInvalidTorrentFile)

	var torrents, files, stored int
	require.NoError(t, pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM torrents), count(*), max(octet_length(path)) FROM torrent_files`).Scan(&torrents, &files, &stored))
	require.Equal(t, []int{1, 1, metainfo.MaxPathLen}, []int{torrents, files, stored})
}
