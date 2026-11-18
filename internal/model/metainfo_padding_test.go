package model_test

import (
	"crypto/sha1"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
	"github.com/stretchr/testify/require"
)

func TestTorrentFromMetadataSkipsBEP47PaddingAndPreservesOriginalIndexes(t *testing.T) {
	piece := make([]byte, 16384)
	sum := sha1.Sum(piece)
	// 2 real files separated by a BEP 47 .pad file and an attr="p" file
	rawInfo, err := bencode.Marshal(map[string]any{
		"name":         "Synthetic.Album.FLAC",
		"piece length": 16384,
		"pieces":       sum[:],
		"files": []map[string]any{
			{"length": 5000, "path": []string{"01-Track1.flac"}},
			{"length": 3192, "path": []string{".pad", "3192"}},
			{"length": 6000, "path": []string{"02-Track2.flac"}},
			{"length": 2192, "attr": "p", "path": []string{"padding.bin"}},
		},
	})
	require.NoError(t, err)
	ih := sha1.Sum(rawInfo)
	validated, err := metainfo.ParseInfoBytes(protocol.InfoHashV1(protocol.ID(ih)), rawInfo)
	require.NoError(t, err)

	projected := model.TorrentFromMetadata(validated, "dht", false, 2)
	require.Equal(t, model.FilesStatusMulti, projected.FilesStatus, "padding entries must not trip fileLimit=2 when only 2 real files exist")
	require.True(t, projected.FilesCount.Valid)
	require.Equal(t, uint(2), projected.FilesCount.Uint)
	require.Equal(t, uint(11000), projected.Size, "size reflects real non-padding payload bytes")
	require.Len(t, projected.Files, 2)
	require.Equal(t, uint(0), projected.Files[0].Index)
	require.Equal(t, "01-Track1.flac", projected.Files[0].Path)
	require.Equal(t, uint(2), projected.Files[1].Index, "original metainfo file index is preserved across skipped padding")
	require.Equal(t, "02-Track2.flac", projected.Files[1].Path)
}
