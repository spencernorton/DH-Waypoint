package csamblocklistfx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencernorton/DH-Waypoint/internal/csamblocklist"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"go.uber.org/zap"
)

// runStartupRefresh — when called with a populated feed, the active
// bloom contains the configured feed's entries by the time the call
// returns (review fix: "Initial blocklist refresh does not block
// crawler startup").
func TestRunStartupRefresh_BlocksUntilFirstFetchCompletes(t *testing.T) {
	var bad protocol.InfoHash
	for i := range bad {
		bad[i] = byte(0x55)
	}

	// Feed waits 200ms before serving — gives a clear gap to verify
	// the helper actually waits.
	var fetched atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(csamblocklist.DoubleHashHex(bad) + "\n"))
		fetched.Add(1)
	}))
	defer srv.Close()

	cfg := csamblocklist.NewDefaultConfig()
	cfg.FeedUrls = []string{srv.URL}
	cfg.FeedFetchTimeout = 5 * time.Second
	logger := zap.NewNop().Sugar()
	mgr := csamblocklist.NewService(cfg, logger, csamblocklist.NewMetrics())

	startTime := time.Now()
	if err := runStartupRefresh(context.Background(), mgr, logger, 5*time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	elapsed := time.Since(startTime)

	if elapsed < 150*time.Millisecond {
		t.Errorf("runStartupRefresh returned in %v — expected to block on initial refresh (>= 200ms)", elapsed)
	}
	if got := fetched.Load(); got < 1 {
		t.Errorf("feed not fetched: count=%d", got)
	}
	if !mgr.IsBlocked(bad) {
		t.Fatal("after runStartupRefresh, expected bad infohash to be blocked")
	}
}

// runStartupRefresh — when the feed hangs, the call returns within
// the configured budget rather than blocking forever. The active
// bloom remains empty, but the app is allowed to boot (fail-open).
//
// The handler honours the request's own context so that when the
// outer fetch ctx times out (FeedFetchTimeout shorter than the
// budget), the handler exits cleanly. This avoids a deferred
// httptest.Server.Close() blocking on an in-flight handler.
func TestRunStartupRefresh_BoundedByBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	cfg := csamblocklist.NewDefaultConfig()
	cfg.FeedUrls = []string{srv.URL}
	// Per-feed timeout shorter than the outer budget so the fetch
	// gives up cleanly before the OnStart budget elapses, AND the
	// handler's request context cancels cleanly when the client
	// hangs up.
	cfg.FeedFetchTimeout = 200 * time.Millisecond
	logger := zap.NewNop().Sugar()
	mgr := csamblocklist.NewService(cfg, logger, csamblocklist.NewMetrics())

	startTime := time.Now()
	if err := runStartupRefresh(context.Background(), mgr, logger, 2*time.Second); err != nil {
		t.Fatalf("external feed timeout without local store must remain fail-open, got: %v", err)
	}
	elapsed := time.Since(startTime)

	// Should give up well within the outer budget — the per-feed
	// fetch timeout fires first.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("returned too late: %v > 1.5s (budget=2s, fetch_timeout=200ms)", elapsed)
	}

	// Active bloom is empty (no feed succeeded).
	var ih protocol.InfoHash
	if mgr.IsBlocked(ih) {
		t.Error("expected empty bloom after failed initial refresh")
	}
}

// runStartupRefresh — a NoOp manager returns immediately without doing
// any work.
func TestRunStartupRefresh_NoOpReturnsImmediately(t *testing.T) {
	logger := zap.NewNop().Sugar()
	mgr := csamblocklist.NewNoOp()

	startTime := time.Now()
	if err := runStartupRefresh(context.Background(), mgr, logger, 5*time.Second); err != nil {
		t.Fatalf("NoOp.runStartupRefresh returned error: %v", err)
	}
	elapsed := time.Since(startTime)

	if elapsed > 50*time.Millisecond {
		t.Errorf("NoOp.runStartupRefresh took %v — expected <50ms", elapsed)
	}
}

// runStartupRefresh — when backed by PostgreSQL csam_local_observations, a
// cold-start load failure must fail closed rather than booting with an empty bloom.
func TestRunStartupRefresh_FailsClosedOnLocalStoreError(t *testing.T) {
	logger := zap.NewNop().Sugar()
	cfg := csamblocklist.NewDefaultConfig()
	cfg.Enabled = false // local-only mode with external feeds disabled
	brokenPool := lazy.New(func() (*pgxpool.Pool, error) {
		return nil, errors.New("postgres unavailable")
	})
	mgr := csamblocklist.New(cfg, logger, csamblocklist.NewMetrics(), brokenPool, nil)

	err := runStartupRefresh(context.Background(), mgr, logger, 2*time.Second)
	if !errors.Is(err, csamblocklist.ErrLocalObservationsLoad) {
		t.Fatalf("expected ErrLocalObservationsLoad fail-closed error, got: %v", err)
	}
}
