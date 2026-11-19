package metrics

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResolveWindow(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	t.Run("defaults when start and end omitted", func(t *testing.T) {
		for _, tc := range []struct {
			dur  BucketDuration
			span time.Duration
		}{
			{BucketDurationMinute, DefaultMinuteWindow},
			{BucketDurationHour, DefaultHourWindow},
			{BucketDurationDay, DefaultDayWindow},
		} {
			win, err := ResolveWindow(tc.dur, time.Time{}, time.Time{}, now)
			require.NoError(t, err)
			require.False(t, win.Empty)
			require.Equal(t, now.Add(-tc.span), win.Start)
			require.True(t, win.End.IsZero())
		}
	})

	t.Run("default lookback anchors to explicit end", func(t *testing.T) {
		end := now.Add(-48 * time.Hour)
		win, err := ResolveWindow(BucketDurationMinute, time.Time{}, end, now)
		require.NoError(t, err)
		require.Equal(t, end.Add(-DefaultMinuteWindow), win.Start)
		require.Equal(t, end, win.End)
	})

	t.Run("clamps excessive span to granularity max", func(t *testing.T) {
		for _, tc := range []struct {
			dur    BucketDuration
			maxWin time.Duration
		}{
			{BucketDurationMinute, MaxMinuteWindow},
			{BucketDurationHour, MaxHourWindow},
			{BucketDurationDay, MaxDayWindow},
		} {
			oldStart := now.Add(-10 * 365 * 24 * time.Hour)
			win, err := ResolveWindow(tc.dur, oldStart, now, now)
			require.NoError(t, err)
			require.False(t, win.Empty)
			require.Equal(t, now.Add(-tc.maxWin), win.Start)
			require.Equal(t, now, win.End)
		}
	})

	t.Run("preserves normal dashboard range unchanged", func(t *testing.T) {
		start := now.Add(-6 * time.Hour)
		win, err := ResolveWindow(BucketDurationMinute, start, now, now)
		require.NoError(t, err)
		require.False(t, win.Empty)
		require.Equal(t, start, win.Start)
		require.Equal(t, now, win.End)
	})

	t.Run("inverted range returns empty without error", func(t *testing.T) {
		win, err := ResolveWindow(BucketDurationHour, now, now.Add(-time.Hour), now)
		require.NoError(t, err)
		require.True(t, win.Empty)
	})

	t.Run("invalid bucket duration rejected", func(t *testing.T) {
		_, err := ResolveWindow("second", time.Time{}, time.Time{}, now)
		require.Error(t, err)
		_, err = ResolveWindow("", time.Time{}, time.Time{}, now)
		require.Error(t, err)
	})
}
