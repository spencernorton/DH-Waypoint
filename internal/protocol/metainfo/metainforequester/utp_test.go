package metainforequester

import (
	"context"
	"crypto/sha1"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	utp "github.com/anacrolix/go-utp"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/mse"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo/peerrep"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func startUTPMetadataPeer(t *testing.T, infoBytes []byte, infoHash protocol.ID, requireMSE bool) (netip.AddrPort, <-chan struct{}) {
	t.Helper()
	sock, err := utp.Listen(utp.PureGo, "udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = sock.Close() })

	accepted := make(chan struct{}, 8)
	go func() {
		for {
			c, accErr := sock.Accept()
			if accErr != nil {
				return
			}
			select {
			case accepted <- struct{}{}:
			default:
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				if !requireMSE {
					serveSyntheticBEP9(conn, infoHash, infoBytes)
					return
				}
				var first [1]byte
				if _, rErr := io.ReadFull(conn, first[:]); rErr != nil {
					return
				}
				if first[0] == 0x13 {
					return
				}
				prefixed := &prefixedConn{Conn: conn, first: first[:]}
				rw, _, hsErr := mse.ReceiveHandshake(context.Background(), prefixed, func(f func([]byte) bool) {
					f(infoHash[:])
				}, mse.DefaultCryptoSelector)
				if hsErr != nil {
					return
				}
				serveSyntheticBEP9(rw, infoHash, infoBytes)
			}(c)
		}
	}()

	return sock.Addr().(*net.UDPAddr).AddrPort(), accepted
}

func syntheticUTPInfo(t *testing.T, name string) ([]byte, protocol.ID) {
	t.Helper()
	piece := sha1.Sum([]byte("synthetic utp test payload " + name))
	infoBytes, err := bencode.Marshal(map[string]any{
		"name":         name,
		"piece length": int64(16384),
		"length":       int64(32),
		"pieces":       piece[:],
	})
	require.NoError(t, err)
	return infoBytes, protocol.ID(sha1.Sum(infoBytes))
}

func TestRequesterUTPFallbackPlainAndMSE(t *testing.T) {
	t.Run("utp_only_plaintext", func(t *testing.T) {
		infoBytes, infoHash := syntheticUTPInfo(t, "Synthetic.UTP.Plain.mkv")
		addr, _ := startUTPMetadataPeer(t, infoBytes, infoHash, false)

		// Default EnableUTP=false must fail against a uTP-only peer.
		tcpOnly := New(Params{
			Config:        Config{RequestTimeout: 2 * time.Second, AllowPrivateAddrs: true, EnableUTP: false},
			PeerRepConfig: peerrep.NewDefaultConfig(),
			Logger:        zap.NewNop().Sugar(),
		})
		t.Cleanup(func() { _ = tcpOnly.AppHook.OnStop(context.Background()) })
		_, err := tcpOnly.Requester.Request(context.Background(), infoHash, addr)
		require.Error(t, err)

		// Opt-in EnableUTP=true recovers metadata within one shared budget.
		utpEnabled := New(Params{
			Config:        Config{RequestTimeout: 4 * time.Second, AllowPrivateAddrs: true, EnableUTP: true},
			PeerRepConfig: peerrep.NewDefaultConfig(),
			Logger:        zap.NewNop().Sugar(),
		})
		t.Cleanup(func() { _ = utpEnabled.AppHook.OnStop(context.Background()) })
		res, err := utpEnabled.Requester.Request(context.Background(), infoHash, addr)
		require.NoError(t, err)
		require.Equal(t, "Synthetic.UTP.Plain.mkv", res.Info.Name)
		require.Equal(t, infoBytes, res.Validated.InfoBytes)

		reg := prometheus.NewRegistry()
		require.NoError(t, reg.Register(utpEnabled.RequestTransportTotal))
		mfs, err := reg.Gather()
		require.NoError(t, err)
		var sawUTPOk bool
		for _, mf := range mfs {
			for _, m := range mf.GetMetric() {
				var tr, out string
				for _, lp := range m.GetLabel() {
					if lp.GetName() == "transport" {
						tr = lp.GetValue()
					}
					if lp.GetName() == "outcome" {
						out = lp.GetValue()
					}
				}
				if tr == "utp_plain" && out == "ok" && m.GetCounter().GetValue() >= 1 {
					sawUTPOk = true
				}
			}
		}
		require.True(t, sawUTPOk, "expected utp_plain/ok transport counter increment")
	})

	t.Run("utp_only_mse_required", func(t *testing.T) {
		infoBytes, infoHash := syntheticUTPInfo(t, "Synthetic.UTP.MSE.mkv")
		addr, _ := startUTPMetadataPeer(t, infoBytes, infoHash, true)

		// EnableUTP without EnableMSE must fail against a uTP+MSE-required peer.
		utpPlainOnly := New(Params{
			Config:        Config{RequestTimeout: 3 * time.Second, AllowPrivateAddrs: true, EnableUTP: true, EnableMSE: false},
			PeerRepConfig: peerrep.NewDefaultConfig(),
			Logger:        zap.NewNop().Sugar(),
		})
		t.Cleanup(func() { _ = utpPlainOnly.AppHook.OnStop(context.Background()) })
		_, err := utpPlainOnly.Requester.Request(context.Background(), infoHash, addr)
		require.Error(t, err)

		// EnableUTP + EnableMSE succeeds over utp_mse within one shared budget.
		both := New(Params{
			Config:        Config{RequestTimeout: 4 * time.Second, AllowPrivateAddrs: true, EnableUTP: true, EnableMSE: true},
			PeerRepConfig: peerrep.NewDefaultConfig(),
			Logger:        zap.NewNop().Sugar(),
		})
		t.Cleanup(func() { _ = both.AppHook.OnStop(context.Background()) })
		res, err := both.Requester.Request(context.Background(), infoHash, addr)
		require.NoError(t, err)
		require.Equal(t, "Synthetic.UTP.MSE.mkv", res.Info.Name)
		require.GreaterOrEqual(t, testutil.CollectAndCount(both.RequestTransportTotal), 3)
	})
}

func TestRequesterUTPRefusesNonPublicPeers(t *testing.T) {
	infoBytes, infoHash := syntheticUTPInfo(t, "Synthetic.UTP.SSRF.mkv")
	addr, accepted := startUTPMetadataPeer(t, infoBytes, infoHash, false)

	guarded := New(Params{
		Config: Config{
			RequestTimeout:    2 * time.Second,
			AllowPrivateAddrs: false,
			EnableMSE:         true,
			EnableUTP:         true,
		},
		PeerRepConfig: peerrep.NewDefaultConfig(),
		Logger:        zap.NewNop().Sugar(),
	})
	t.Cleanup(func() { _ = guarded.AppHook.OnStop(context.Background()) })

	_, err := guarded.Requester.Request(context.Background(), infoHash, addr)
	require.ErrorIs(t, err, errNonPublicAddress)

	select {
	case <-accepted:
		t.Fatal("uTP fallback must never send packets to loopback/private addresses")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestRequesterUTPRespectsSingleBudgetAndCancel(t *testing.T) {
	_, infoHash := syntheticUTPInfo(t, "Synthetic.UTP.Budget.mkv")

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, accErr := ln.Accept()
			if accErr != nil {
				return
			}
			_ = c.Close()
		}
	}()
	addr := ln.Addr().(*net.TCPAddr).AddrPort()

	r := New(Params{
		Config: Config{
			RequestTimeout:    500 * time.Millisecond,
			AllowPrivateAddrs: true,
			EnableMSE:         true,
			EnableUTP:         true,
		},
		PeerRepConfig: peerrep.NewDefaultConfig(),
		Logger:        zap.NewNop().Sugar(),
	})
	t.Cleanup(func() { _ = r.AppHook.OnStop(context.Background()) })

	start := time.Now()
	_, reqErr := r.Requester.Request(context.Background(), infoHash, addr)
	require.Error(t, reqErr)
	require.Less(t, time.Since(start), 2*time.Second, "TCP+MSE+uTP fallback must share one RequestTimeout budget")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, cancelErr := r.Requester.Request(ctx, infoHash, addr)
	require.Error(t, cancelErr)
}
