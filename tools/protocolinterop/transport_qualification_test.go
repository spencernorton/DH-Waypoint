package protocolinterop

import (
	"context"
	"crypto/sha1"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	utp "github.com/anacrolix/go-utp"
	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	mi "github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/mse"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	mreq "github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo/metainforequester"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// transportProfile defines one isolated local-peer transport configuration for
// measuring baseline TCP-only requester behavior vs audited library adapters
// (go-utp PureGo + anacrolix/torrent/mse) under a single shared deadline.
type transportProfile struct {
	name             string
	disableTCP       bool
	disableUTP       bool
	requireMSE       bool
	baselineTCPWorks bool
	wantTransport    string
}

func syntheticV1Metadata(t *testing.T, name string) ([]byte, protocol.ID) {
	t.Helper()
	data := []byte("synthetic transport qualification payload")
	piece := sha1.Sum(data)
	raw, err := bencode.Marshal(map[string]any{
		"name":         name,
		"piece length": int64(16384),
		"length":       int64(len(data)),
		"pieces":       piece[:],
	})
	require.NoError(t, err)
	return raw, protocol.ID(sha1.Sum(raw))
}

func startPeerForProfile(t *testing.T, p transportProfile, raw []byte, wireHash protocol.ID) (netip.AddrPort, *torrent.Torrent) {
	t.Helper()
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	cfg.SetListenAddr("127.0.0.1:0")
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.DisablePEX = true
	cfg.NoDefaultPortForwarding = true
	cfg.DisableIPv6 = true
	cfg.AlwaysWantConns = true
	cfg.DisableTCP = p.disableTCP
	cfg.DisableUTP = p.disableUTP
	if p.requireMSE {
		cfg.HeaderObfuscationPolicy = torrent.HeaderObfuscationPolicy{
			RequirePreferred: true,
			Preferred:        true,
		}
	} else {
		cfg.HeaderObfuscationPolicy = torrent.HeaderObfuscationPolicy{
			RequirePreferred: true,
			Preferred:        false,
		}
	}
	client, err := torrent.NewClient(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })

	var miHash mi.Hash
	copy(miHash[:], wireHash[:])
	pt, _ := client.AddTorrentInfoHash(miHash)
	require.NoError(t, pt.SetInfoBytes(raw))
	pt.DownloadAll()

	var addr netip.AddrPort
	for _, cand := range client.ListenAddrs() {
		switch a := cand.(type) {
		case *net.TCPAddr:
			if !p.disableTCP {
				addr = a.AddrPort()
			}
		case *net.UDPAddr:
			if p.disableTCP && !p.disableUTP {
				addr = a.AddrPort()
			}
		}
		if addr.IsValid() {
			break
		}
	}
	require.True(t, addr.IsValid(), "expected valid listen address for profile %s", p.name)
	return addr, pt
}

// qualifiedFallbackFetch exercises a bounded, sequential single-budget transport
// fallback (TCP plaintext -> TCP MSE/PE -> uTP plaintext -> uTP MSE/PE) using
// audited library adapters without modifying production requester defaults.
func qualifiedFallbackFetch(ctx context.Context, utpSock utp.Socket, infoHash protocol.ID, addr netip.AddrPort, budget time.Duration) (mreq.Response, string, error) {
	overallCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	clientID := protocol.ID{0x51, 0x55}
	dialer := &net.Dialer{}

	tryTCP := func(encrypt bool) (mreq.Response, error) {
		conn, err := dialer.DialContext(overallCtx, "tcp", addr.String())
		if err != nil {
			return mreq.Response{}, err
		}
		if dl, ok := overallCtx.Deadline(); ok {
			_ = conn.SetDeadline(dl)
		}
		var rw io.ReadWriter = conn
		if encrypt {
			stopClose := context.AfterFunc(overallCtx, func() { _ = conn.Close() })
			encRW, _, mseErr := mse.InitiateHandshake(conn, infoHash[:], nil, mse.AllSupportedCrypto)
			stopClose()
			if mseErr != nil {
				_ = conn.Close()
				return mreq.Response{}, mseErr
			}
			rw = encRW
		}
		return mreq.FetchFromStream(overallCtx, conn, rw, infoHash, clientID)
	}

	tryUTP := func(encrypt bool) (mreq.Response, error) {
		if utpSock == nil {
			return mreq.Response{}, errors.New("utp socket unavailable")
		}
		conn, err := utpSock.DialContext(overallCtx, "", addr.String())
		if err != nil {
			return mreq.Response{}, err
		}
		if dl, ok := overallCtx.Deadline(); ok {
			_ = conn.SetDeadline(dl)
		}
		var rw io.ReadWriter = conn
		if encrypt {
			stopClose := context.AfterFunc(overallCtx, func() { _ = conn.Close() })
			encRW, _, mseErr := mse.InitiateHandshake(conn, infoHash[:], nil, mse.AllSupportedCrypto)
			stopClose()
			if mseErr != nil {
				_ = conn.Close()
				return mreq.Response{}, mseErr
			}
			rw = encRW
		}
		return mreq.FetchFromStream(overallCtx, conn, rw, infoHash, clientID)
	}

	attempts := []struct {
		label string
		fn    func() (mreq.Response, error)
	}{
		{"tcp_plaintext", func() (mreq.Response, error) { return tryTCP(false) }},
		{"tcp_mse", func() (mreq.Response, error) { return tryTCP(true) }},
		{"utp_plaintext", func() (mreq.Response, error) { return tryUTP(false) }},
		{"utp_mse", func() (mreq.Response, error) { return tryUTP(true) }},
	}

	var lastErr error
	for _, attempt := range attempts {
		if overallCtx.Err() != nil {
			return mreq.Response{}, "", overallCtx.Err()
		}
		res, err := attempt.fn()
		if err == nil {
			return res, attempt.label, nil
		}
		lastErr = err
	}
	return mreq.Response{}, "", lastErr
}

func TestTransportQualificationMatrixTCPUTPMSE(t *testing.T) {
	profiles := []transportProfile{
		{
			name:             "tcp_only_plaintext",
			disableTCP:       false,
			disableUTP:       true,
			requireMSE:       false,
			baselineTCPWorks: true,
			wantTransport:    "tcp_plaintext",
		},
		{
			name:             "tcp_mse_required",
			disableTCP:       false,
			disableUTP:       true,
			requireMSE:       true,
			baselineTCPWorks: false,
			wantTransport:    "tcp_mse",
		},
		{
			name:             "utp_only_plaintext",
			disableTCP:       true,
			disableUTP:       false,
			requireMSE:       false,
			baselineTCPWorks: false,
			wantTransport:    "utp_plaintext",
		},
		{
			name:             "utp_only_mse_required",
			disableTCP:       true,
			disableUTP:       false,
			requireMSE:       true,
			baselineTCPWorks: false,
			wantTransport:    "utp_mse",
		},
	}

	utpSock, err := utp.Listen(utp.PureGo, "udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = utpSock.Close() })

	baseline := mreq.New(mreq.Params{
		Config: mreq.Config{
			RequestTimeout:    1500 * time.Millisecond,
			KeyMutexSize:      100,
			AllowPrivateAddrs: true,
		},
		Logger: zap.NewNop().Sugar(),
	}).Requester

	for _, p := range profiles {
		t.Run(p.name, func(t *testing.T) {
			raw, wireHash := syntheticV1Metadata(t, "Synthetic."+p.name+".mkv")
			addr, pt := startPeerForProfile(t, p, raw, wireHash)

			baseRes, baseErr := baseline.Request(context.Background(), wireHash, addr)
			if p.baselineTCPWorks {
				require.NoError(t, baseErr)
				require.Equal(t, raw, baseRes.Validated.InfoBytes)
			} else {
				require.Error(t, baseErr, "baseline TCP-only requester must fail against %s", p.name)
			}

			start := time.Now()
			res, used, fbErr := qualifiedFallbackFetch(context.Background(), utpSock, wireHash, addr, 4*time.Second)
			require.NoError(t, fbErr)
			require.Equal(t, p.wantTransport, used)
			require.Equal(t, raw, res.Validated.InfoBytes)
			require.Less(t, time.Since(start), 4*time.Second)
			stats := pt.Stats()
			require.Zero(t, stats.BytesReadData.Int64(), "qualification fixture must never transfer payload data")
		})
	}
}

func TestTransportQualificationRefusingMalformedAndCanceledPeers(t *testing.T) {
	utpSock, err := utp.Listen(utp.PureGo, "udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = utpSock.Close() })
	_, wireHash := syntheticV1Metadata(t, "Synthetic.Hostile.mkv")

	t.Run("refusing_peer_bounds_single_budget", func(t *testing.T) {
		ln, listenErr := net.Listen("tcp4", "127.0.0.1:0")
		require.NoError(t, listenErr)
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
		start := time.Now()
		_, _, fetchErr := qualifiedFallbackFetch(context.Background(), utpSock, wireHash, addr, 400*time.Millisecond)
		require.Error(t, fetchErr)
		require.Less(t, time.Since(start), 2*time.Second, "fallback attempts must share one overall timeout budget")
	})

	t.Run("malformed_handshake_peer_fails_cleanly_within_budget", func(t *testing.T) {
		ln, listenErr := net.Listen("tcp4", "127.0.0.1:0")
		require.NoError(t, listenErr)
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				c, accErr := ln.Accept()
				if accErr != nil {
					return
				}
				_, _ = c.Write([]byte("NOT-BITTORRENT-NOT-MSE-GARBAGE\x00\xff\xfe"))
				_ = c.Close()
			}
		}()

		addr := ln.Addr().(*net.TCPAddr).AddrPort()
		start := time.Now()
		_, _, fetchErr := qualifiedFallbackFetch(context.Background(), utpSock, wireHash, addr, 400*time.Millisecond)
		require.Error(t, fetchErr)
		require.Less(t, time.Since(start), 2*time.Second)
	})

	t.Run("canceled_context_interrupts_stalled_mse_and_utp_attempts", func(t *testing.T) {
		ln, listenErr := net.Listen("tcp4", "127.0.0.1:0")
		require.NoError(t, listenErr)
		release := make(chan struct{})
		t.Cleanup(func() { close(release); _ = ln.Close() })
		accepted := make(chan struct{}, 4)
		go func() {
			for {
				c, accErr := ln.Accept()
				if accErr != nil {
					return
				}
				select {
				case accepted <- struct{}{}:
				default:
				}
				go func(conn net.Conn) {
					<-release
					_ = conn.Close()
				}(c)
			}
		}()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		addr := ln.Addr().(*net.TCPAddr).AddrPort()
		done := make(chan error, 1)
		go func() {
			_, _, fetchErr := qualifiedFallbackFetch(ctx, utpSock, wireHash, addr, 5*time.Second)
			done <- fetchErr
		}()

		select {
		case <-accepted:
		case <-time.After(time.Second):
			t.Fatal("stalled peer never received connection")
		}
		cancel()
		select {
		case fetchErr := <-done:
			require.Error(t, fetchErr)
		case <-time.After(time.Second):
			t.Fatal("canceled transport fallback remained blocked on stalled peer")
		}
	})
}
