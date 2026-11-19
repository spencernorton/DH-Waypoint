package metrics

import (
	"fmt"
	"time"
)

// BucketDuration represents the duration of bucketing for metrics queries.
// ENUM(minute, hour, day)
type BucketDuration string

const (
	BucketDurationMinute BucketDuration = "minute"
	BucketDurationHour   BucketDuration = "hour"
	BucketDurationDay    BucketDuration = "day"

	// MaxBuckets caps the maximum grouped rows returned by a single non-paged
	// metrics query so wide or unbounded GraphQL requests cannot exhaust memory.
	MaxBuckets = 5000

	// Default lookback windows applied when StartTime is omitted, sized to
	// preserve dashboard views without scanning full multi-year history.
	DefaultMinuteWindow = 24 * time.Hour
	DefaultHourWindow   = 30 * 24 * time.Hour
	DefaultDayWindow    = 365 * 24 * time.Hour

	// Maximum time spans allowed per bucket granularity before clamping
	// StartTime forward relative to the effective end time.
	MaxMinuteWindow = 7 * 24 * time.Hour
	MaxHourWindow   = 180 * 24 * time.Hour
	MaxDayWindow    = 1825 * 24 * time.Hour
)

// TimeWindow holds normalized, bounded metrics query bounds.
type TimeWindow struct {
	Start time.Time
	End   time.Time
	Empty bool
}

// ResolveWindow validates duration, applies a default lookback when start is
// omitted, clamps excessive spans to the granularity maximum, and flags
// inverted [start, end] ranges as Empty so callers can return immediately.
func ResolveWindow(duration BucketDuration, start, end, now time.Time) (TimeWindow, error) {
	var defaultSpan, maxSpan time.Duration
	switch duration {
	case BucketDurationMinute:
		defaultSpan = DefaultMinuteWindow
		maxSpan = MaxMinuteWindow
	case BucketDurationHour:
		defaultSpan = DefaultHourWindow
		maxSpan = MaxHourWindow
	case BucketDurationDay:
		defaultSpan = DefaultDayWindow
		maxSpan = MaxDayWindow
	default:
		return TimeWindow{}, fmt.Errorf("metrics: invalid bucket duration %q", duration)
	}

	if !start.IsZero() && !end.IsZero() && start.After(end) {
		return TimeWindow{Empty: true}, nil
	}

	effectiveEnd := end
	if effectiveEnd.IsZero() {
		effectiveEnd = now.UTC()
	}
	effectiveStart := start
	if effectiveStart.IsZero() {
		effectiveStart = effectiveEnd.Add(-defaultSpan)
	} else if effectiveEnd.Sub(effectiveStart) > maxSpan {
		effectiveStart = effectiveEnd.Add(-maxSpan)
	}

	return TimeWindow{
		Start: effectiveStart,
		End:   end,
	}, nil
}
