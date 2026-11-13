package tmdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestRequesterNeverLogsAPIKey(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // every attempt fails to connect: a *url.Error whose message is the full request URL

	core, logs := observer.New(zapcore.DebugLevel)
	client := newRestyClient(Config{BaseURL: srv.URL, APIKey: "secret123"}, zap.New(core).Sugar()).
		SetDebug(true).
		SetRetryWaitTime(time.Millisecond).
		SetRetryMaxWaitTime(time.Millisecond)

	_, err := requester{resty: client}.Request(context.Background(), "/authentication", nil, nil)
	require.Error(t, err)

	lines := []string{err.Error()}

	for _, level := range []zapcore.Level{zapcore.DebugLevel, zapcore.WarnLevel, zapcore.ErrorLevel} {
		entries := logs.FilterLevelExact(level).All()
		require.NotEmpty(t, entries, "resty logged nothing at %s", level)

		for _, e := range entries {
			lines = append(lines, e.Message)
		}
	}

	for _, line := range lines {
		assert.Contains(t, line, "api_key=REDACTED")
		assert.NotContains(t, line, "secret123")
	}
}

func TestDefaultConfigHasNoSharedCredential(t *testing.T) {
	require.Empty(t, NewDefaultConfig().APIKey)
}

func TestMissingKeyDoesNotContactProvider(t *testing.T) {
	config := NewDefaultConfig()
	config.BaseURL = "http://127.0.0.1:1"
	_, err := newRequester(context.Background(), config, zap.NewNop().Sugar())
	require.ErrorContains(t, err, "TMDB_API_KEY must be configured")
}

func TestUnauthorizedKeyDoesNotFallBack(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "example-key", r.URL.Query().Get("api_key"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	config := NewDefaultConfig()
	config.APIKey = "example-key"
	config.BaseURL = server.URL
	_, err := newRequester(context.Background(), config, zap.NewNop().Sugar())
	require.ErrorIs(t, err, ErrUnauthorized)
	require.Equal(t, 1, calls)
}

// lazyAgainst returns a lazy requester for a provider that answers its first
// len(first) requests with those statuses and every later one with 200.
func lazyAgainst(t *testing.T, first ...int) (*requesterLazy, *atomic.Int32, *observer.ObservedLogs) {
	t.Helper()

	calls := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n := int(calls.Add(1)); n <= len(first) {
			w.WriteHeader(first[n-1])
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	config := NewDefaultConfig()
	config.APIKey = "example-key"
	config.BaseURL = server.URL
	core, logs := observer.New(zapcore.WarnLevel)

	return &requesterLazy{config: config, logger: zap.New(core).Sugar()}, calls, logs
}

// A first use that fails for any reason other than a rejected key is not
// remembered: the next call reaches the provider again.
func TestFirstUseFailureIsNotRemembered(t *testing.T) {
	// The key check and the first call's own request both fail.
	r, calls, logs := lazyAgainst(t, http.StatusServiceUnavailable, http.StatusServiceUnavailable)

	_, err := r.Request(context.Background(), "/authentication", nil, nil)
	require.Error(t, err)

	_, err = r.Request(context.Background(), "/authentication", nil, nil)
	require.NoError(t, err)
	require.EqualValues(t, 3, calls.Load())
	require.Equal(t, 1, logs.FilterMessageSnippet("key check").Len())
}

// A rejected key is remembered: later calls fail without another request.
func TestRejectedKeyIsRemembered(t *testing.T) {
	r, calls, _ := lazyAgainst(t, http.StatusUnauthorized)

	for range 3 {
		_, err := r.Request(context.Background(), "/authentication", nil, nil)
		require.ErrorIs(t, err, ErrUnauthorized)
	}
	require.EqualValues(t, 1, calls.Load())
}
