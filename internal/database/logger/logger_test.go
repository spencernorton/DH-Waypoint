package logger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	gormlogger "gorm.io/gorm/logger"
)

func newObservedLogger(level zapcore.Level, gormLevel gormlogger.LogLevel, slow time.Duration) (gormlogger.Interface, *observer.ObservedLogs) {
	core, logs := observer.New(level)
	res := New(Params{
		Config: Config{
			LogLevel:      gormLevel,
			SlowThreshold: slow,
		},
		ZapLogger: zap.New(core).Sugar(),
	})
	return res.GormLogger, logs
}

func TestTraceSkipsExplainSQLWhenZapDebugDisabled(t *testing.T) {
	l, logs := newObservedLogger(zapcore.InfoLevel, gormlogger.Info, 200*time.Millisecond)

	calls := 0
	l.Trace(context.Background(), time.Now(), func() (string, int64) {
		calls++
		return "SELECT 1", 1
	}, nil)

	require.Equal(t, 0, calls, "fc() must not be invoked when zap debug logging is disabled")
	require.Equal(t, 0, logs.Len())
}

func TestTraceEvaluatesExplainSQLWhenZapDebugEnabled(t *testing.T) {
	l, logs := newObservedLogger(zapcore.DebugLevel, gormlogger.Info, 200*time.Millisecond)

	calls := 0
	l.Trace(context.Background(), time.Now(), func() (string, int64) {
		calls++
		return "SELECT 1", 1
	}, nil)

	require.Equal(t, 1, calls)
	require.Equal(t, 1, logs.Len())
	require.Equal(t, zapcore.DebugLevel, logs.All()[0].Level)
}

func TestTraceStillLogsSlowAndErrorQueriesAtInfoLevel(t *testing.T) {
	l, logs := newObservedLogger(zapcore.InfoLevel, gormlogger.Info, 10*time.Millisecond)

	calls := 0
	fc := func() (string, int64) {
		calls++
		return "SELECT slow", 1
	}

	// Ignored errors must not trigger fc().
	l.Trace(context.Background(), time.Now(), fc, gormlogger.ErrRecordNotFound)
	l.Trace(context.Background(), time.Now(), fc, context.Canceled)
	require.Equal(t, 0, calls)

	// Slow query triggers Warn.
	l.Trace(context.Background(), time.Now().Add(-50*time.Millisecond), fc, nil)
	require.Equal(t, 1, calls)
	require.Equal(t, 1, logs.Len())
	require.Equal(t, zapcore.WarnLevel, logs.All()[0].Level)

	// Real error triggers Error.
	l.Trace(context.Background(), time.Now(), fc, errors.New("boom"))
	require.Equal(t, 2, calls)
	require.Equal(t, 2, logs.Len())
	require.Equal(t, zapcore.ErrorLevel, logs.All()[1].Level)
}

func TestLogModeReturnsCopyWithUpdatedLevel(t *testing.T) {
	orig, logs := newObservedLogger(zapcore.InfoLevel, gormlogger.Info, 10*time.Millisecond)
	silent := orig.LogMode(gormlogger.Silent)

	calls := 0
	fc := func() (string, int64) {
		calls++
		return "SELECT err", 0
	}

	silent.Trace(context.Background(), time.Now(), fc, errors.New("boom"))
	require.Equal(t, 0, calls, "silent copy must suppress error tracing")
	require.Equal(t, 0, logs.Len())

	orig.Trace(context.Background(), time.Now(), fc, errors.New("boom"))
	require.Equal(t, 1, calls, "original logger must remain unchanged")
	require.Equal(t, 1, logs.Len())
}
