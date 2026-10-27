package processors

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestDrainRunsReadyStepsUntilIdleOrError(t *testing.T) {
	failed := errors.New("prepare processor job: invalid_binding")
	stale := fmt.Errorf("prepare processor job: %w", ErrStale)
	for _, tc := range []struct {
		name  string
		steps []error // nil = a successful step; the queue is empty afterwards
		calls int
		err   error
	}{
		{name: "empty queue", calls: 1},
		{name: "drains a full queue in one tick", steps: []error{nil, nil, nil}, calls: 4},
		{name: "error waits for the next tick", steps: []error{nil, failed, nil}, calls: 2, err: failed},
		{name: "stale leases keep draining", steps: []error{stale, stale, nil, stale}, calls: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := drain(context.Background(), func(context.Context) (bool, error) {
				calls++
				if calls > len(tc.steps) {
					return false, nil
				}
				return true, tc.steps[calls-1]
			})
			require.ErrorIs(t, err, tc.err)
			require.Equal(t, tc.calls, calls)
		})
	}
}

func TestDrainStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	require.NoError(t, drain(ctx, func(context.Context) (bool, error) {
		calls++
		cancel()
		return true, nil
	}))
	require.Equal(t, 1, calls)
}

func TestStepFailuresReportsAtMostOncePerMinute(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	log := zap.New(core).Sugar()
	var f stepFailures
	start := time.Unix(1_800_000_000, 0)
	err := errors.New("prepare processor job: invalid_binding")
	f.note(log, err, start)
	f.note(log, err, start.Add(10*time.Second))
	f.note(log, err, start.Add(59*time.Second))
	f.note(log, err, start.Add(61*time.Second))

	entries := logs.All()
	require.Len(t, entries, 2)
	require.Equal(t, int64(1), entries[0].ContextMap()["failures"])
	require.Equal(t, int64(3), entries[1].ContextMap()["failures"])
	require.Equal(t, err.Error(), entries[1].ContextMap()["error"])
}
