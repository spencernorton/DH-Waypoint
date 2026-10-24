package protocolinterop

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	mi "github.com/anacrolix/torrent/metainfo"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
	mreq "github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo/metainforequester"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Interoperate with an independent peer implementation, not a second copy of
// the requester's protocol code. Everything binds loopback; no DHT, trackers,
// port forwarding or peer exchange is enabled. Payload counters must stay zero.
func TestMetadataExchangeWithAnacrolixV1V2Hybrid(t *testing.T) {
	for _, kind := range []string{"v1", "v2", "hybrid"} {
		for _, multiBlock := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/multi-block=%t", kind, multiBlock), func(t *testing.T) {
				data := []byte("synthetic data")
				root := sha256.Sum256(data)
				info := map[string]any{"name": "Synthetic.txt", "piece length": int64(16384)}
				if multiBlock {
					info["source"] = strings.Repeat("synthetic", 4096)
				}
				if kind != "v1" {
					info["meta version"] = int64(2)
					info["file tree"] = map[string]any{"Synthetic.txt": map[string]any{"": map[string]any{"length": int64(len(data)), "pieces root": root[:]}}}
				}
				if kind != "v2" {
					piece := sha1.Sum(data)
					info["length"] = int64(len(data))
					info["pieces"] = piece[:]
				}
				raw, err := bencode.Marshal(info)
				require.NoError(t, err)
				cfg := torrent.NewDefaultClientConfig()
				cfg.DataDir = t.TempDir()
				cfg.SetListenAddr("127.0.0.1:0")
				cfg.NoDHT = true
				cfg.DisableTrackers = true
				cfg.DisablePEX = true
				cfg.DisableUTP = true
				cfg.NoDefaultPortForwarding = true
				cfg.DisableIPv6 = true
				client, err := torrent.NewClient(cfg)
				require.NoError(t, err)
				t.Cleanup(func() { client.Close() })
				wireHash := mi.Hash(sha1.Sum(raw))
				if kind == "v2" {
					sum := sha256.Sum256(raw)
					copy(wireHash[:], sum[:20])
				}
				peerTorrent, _ := client.AddTorrentInfoHash(wireHash)
				require.NoError(t, peerTorrent.SetInfoBytes(raw))
				peerTorrent.DownloadAll()
				var addr netip.AddrPort
				for _, candidate := range client.ListenAddrs() {
					if tcp, ok := candidate.(*net.TCPAddr); ok {
						addr = tcp.AddrPort()
						break
					}
				}
				require.True(t, addr.IsValid())
				r := mreq.New(mreq.Params{Config: mreq.Config{RequestTimeout: 3 * time.Second, KeyMutexSize: 1000}, Logger: zap.NewNop().Sugar()}).Requester
				identities := []protocol.InfoHash{}
				if kind != "v2" {
					identities = append(identities, protocol.InfoHashV1(protocol.ID(sha1.Sum(raw))))
				}
				if kind != "v1" {
					identities = append(identities, protocol.InfoHashV2(sha256.Sum256(raw)))
				}
				for _, expected := range identities {
					res, err := mreq.RequestInfoHash(context.Background(), r, expected, addr)
					require.NoError(t, err)
					require.True(t, res.Validated.Identities.Contains(expected))
					require.Equal(t, raw, res.Validated.InfoBytes)
					res, err = r.Request(context.Background(), expected.WireID(), addr)
					require.NoError(t, err)
					require.True(t, res.Validated.Identities.Contains(expected))
				}
				if kind != "v1" {
					expected := identities[len(identities)-1]
					expected[31] ^= 1
					_, err := mreq.RequestInfoHash(context.Background(), r, expected, addr)
					require.ErrorIs(t, err, metainfo.ErrWrongInfoHash)
				}
				stats := peerTorrent.Stats()
				require.Zero(t, stats.BytesReadData.Int64(), "indexer must not send or request payload data")
			})
		}
	}
}
