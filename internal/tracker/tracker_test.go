package tracker

import (
	"bytes"
	"net/url"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	mi "github.com/anacrolix/torrent/metainfo"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
	"github.com/stretchr/testify/require"
)

func TestConfigValidate(t *testing.T) {
	require.NoError(t, NewDefaultConfig().Validate())
	bad := NewDefaultConfig()
	bad.MaxPeers = 100000
	require.Error(t, bad.Validate())
	bad = NewDefaultConfig()
	bad.AnnounceInterval = time.Second
	require.Error(t, bad.Validate())
}

func TestPasskeyAccepts(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Passkeys = []string{"alpha-key", "beta-key"}
	require.True(t, cfg.AcceptsPasskey("beta-key"))
	require.False(t, cfg.AcceptsPasskey("gamma"))
	require.False(t, cfg.AcceptsPasskey(""))
	require.False(t, NewDefaultConfig().AcceptsPasskey("anything"), "no passkeys configured must fail closed")
}

func TestParseAnnounce(t *testing.T) {
	hash := protocol.ID{1, 2, 3}
	q := url.Values{}
	q.Set("info_hash", string(hash.Bytes()))
	q.Set("peer_id", string(bytes.Repeat([]byte{9}, 20)))
	q.Set("port", "6881")
	q.Set("uploaded", "10")
	q.Set("downloaded", "20")
	q.Set("left", "0")
	q.Set("compact", "1")
	q.Set("event", "completed")
	a, err := ParseAnnounce(q)
	require.NoError(t, err)
	require.Equal(t, hash, a.InfoHash)
	require.Equal(t, 6881, a.Port)
	require.True(t, a.Compact)
	require.Equal(t, "completed", a.Event)
	require.Equal(t, DefaultNumWant, a.NumWant)

	q.Set("port", "0")
	_, err = ParseAnnounce(q)
	require.Error(t, err)

	q.Set("port", "6881")
	q.Set("info_hash", "short")
	_, err = ParseAnnounce(q)
	require.Error(t, err)
}

func TestCompactPeersEncoding(t *testing.T) {
	peers := []Peer{
		{IP: "192.0.2.1", Port: 6881},
		{IP: "2001:db8::1", Port: 51413},
		{IP: "not-an-ip", Port: 1},
	}
	v4, v6 := CompactPeers(peers)
	require.Equal(t, []byte{192, 0, 2, 1, 0x1a, 0xe1}, v4)
	require.Len(t, v6, 18)
	require.Equal(t, []byte{0xc8, 0xd5}, v6[16:18])
}

func TestAnnounceResponseBencode(t *testing.T) {
	raw, err := EncodeAnnounce(AnnounceResponse{
		Interval: 1800, Complete: 3, Incomplete: 2,
		Peers:  []byte{127, 0, 0, 1, 0x1a, 0xe1},
		Peers6: []byte{},
	}, true)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, bencode.Unmarshal(raw, &decoded))
	require.EqualValues(t, 1800, decoded["interval"])
	require.EqualValues(t, 3, decoded["complete"])
	require.EqualValues(t, 2, decoded["incomplete"])

	fail, err := EncodeFailure("unregistered torrent")
	require.NoError(t, err)
	decoded = nil
	require.NoError(t, bencode.Unmarshal(fail, &decoded))
	require.Equal(t, "unregistered torrent", decoded["failure reason"])
}

// Rewriting the announce URL for a personalised download must never
// change the info hash: the info dictionary bytes are preserved.
func TestRewriteAnnouncePreservesInfoHash(t *testing.T) {
	priv := true
	info := mi.Info{
		Name:        "synthetic.bin",
		PieceLength: 16384,
		Length:      5,
		Private:     &priv,
		Pieces:      bytes.Repeat([]byte{7}, 20),
	}
	infoBytes, err := bencode.Marshal(info)
	require.NoError(t, err)
	meta := mi.MetaInfo{InfoBytes: infoBytes, Announce: "https://old.example/announce"}
	var buf bytes.Buffer
	require.NoError(t, meta.Write(&buf))
	original := buf.Bytes()
	hash := protocol.ID(mi.HashBytes(infoBytes))

	rewritten, err := RewriteAnnounce(original, "https://tracker.example/announce/KEY")
	require.NoError(t, err)

	parsed, err := metainfo.ParseTorrentFileInfo(hash, rewritten)
	require.NoError(t, err, "rewritten file must still parse under the original hash")
	require.NotNil(t, parsed.Private)
	require.True(t, *parsed.Private)

	loaded, err := mi.Load(bytes.NewReader(rewritten))
	require.NoError(t, err)
	require.Equal(t, "https://tracker.example/announce/KEY", loaded.Announce)

	// Empty announce URL serves the stored bytes verbatim.
	verbatim, err := RewriteAnnounce(original, "")
	require.NoError(t, err)
	require.Equal(t, original, verbatim)
}

func TestInfoHashOfTorrentFile(t *testing.T) {
	info := mi.Info{Name: "x.bin", PieceLength: 16384, Length: 1, Pieces: bytes.Repeat([]byte{3}, 20)}
	infoBytes, err := bencode.Marshal(info)
	require.NoError(t, err)
	meta := mi.MetaInfo{InfoBytes: infoBytes}
	var buf bytes.Buffer
	require.NoError(t, meta.Write(&buf))

	got, parsed, err := ParseTorrentFile(buf.Bytes())
	require.NoError(t, err)
	require.Equal(t, protocol.ID(mi.HashBytes(infoBytes)), got)
	require.Equal(t, "x.bin", parsed.BestName())
}
