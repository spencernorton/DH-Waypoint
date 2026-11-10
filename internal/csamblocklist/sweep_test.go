package csamblocklist

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/stretchr/testify/require"
)

func returns(t *testing.T, run func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		run()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunSweep did not return")
	}
}

// The clean-up deletes by default, and off reads nothing: it returns before
// it asks for a database.
func TestSweepModeDefaultsAndOff(t *testing.T) {
	require.Equal(t, SweepApply, NewDefaultSweepConfig().Mode)
	returns(t, func() {
		NewService(NewDefaultConfig(), nil, NewMetrics()).(*service).RunSweep(context.Background(), SweepOff)
		// No database at all is the same: nothing to read.
		NewService(NewDefaultConfig(), nil, NewMetrics()).(*service).RunSweep(context.Background(), SweepApply)
	})
}

type poolFunc struct {
	lazy.Lazy[*pgxpool.Pool]
	get func() (*pgxpool.Pool, error)
}

func (p poolFunc) Get() (*pgxpool.Pool, error) { return p.get() }

// A failed batch is counted and tried again until the worker stops.
func TestRunSweepRetriesAFailedBatchUntilStopped(t *testing.T) {
	retry := sweepRetry
	sweepRetry = time.Millisecond
	t.Cleanup(func() { sweepRetry = retry })

	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	svc := New(NewDefaultConfig(), nil, NewMetrics(), poolFunc{get: func() (*pgxpool.Pool, error) {
		if calls.Add(1) == 3 {
			cancel()
		}
		return nil, errors.New("database unavailable")
	}}, nil).(*service)
	returns(t, func() { svc.RunSweep(ctx, SweepApply) })
	require.EqualValues(t, 3, calls.Load())
	require.Equal(t, 2.0, testutil.ToFloat64(svc.metrics.SweepBatchErrorsTotal), "the batch that failed because the worker stopped is not an error")
}
