//go:build unix

package workercmd_test

import (
	"context"
	"sync"
	"syscall"
	"testing"
	"time"

	appcli "github.com/spencernorton/DH-Waypoint/internal/app/cli"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/workercmd"
	"github.com/spencernorton/DH-Waypoint/internal/worker"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// A stop signal must run the worker stop hooks with a usable context, and
// before the app-level hooks that close what the workers depend on.
func TestStopSignalRunsWorkerStopHooks(t *testing.T) {
	var (
		mutex      sync.Mutex
		order      []string
		workerStop error
	)
	record := func(name string) {
		mutex.Lock()
		defer mutex.Unlock()
		order = append(order, name)
	}
	started := make(chan struct{})
	synthetic := worker.NewWorker("synthetic", fx.Hook{
		OnStart: func(context.Context) error {
			close(started)
			return nil
		},
		OnStop: func(ctx context.Context) error {
			// Stands in for an HTTP drain or a final database write.
			select {
			case <-ctx.Done():
				workerStop = ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			record("worker")
			return nil
		},
	})
	app := fx.New(
		fx.NopLogger,
		fx.Supply(
			fx.Annotate([]string{"dh-waypoint", "worker", "run", "--all"}, fx.ResultTags(`name:"cli_args"`)),
			zap.NewNop().Sugar(),
			fx.Annotate(fx.Hook{OnStop: func(context.Context) error {
				record("app")
				return nil
			}}, fx.ResultTags(`group:"app_hooks"`)),
		),
		fx.Provide(
			fx.Annotate(func() worker.Worker { return synthetic }, fx.ResultTags(`group:"workers"`)),
			appcli.New, appcli.AttachHooks, worker.NewRegistry, workercmd.New,
		),
		fx.Invoke(func(*cli.App, appcli.AttachedHooks) {}),
	)
	require.NoError(t, app.Err())

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		app.Run()
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not start")
	}
	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGTERM))
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("app did not stop after SIGTERM")
	}

	mutex.Lock()
	defer mutex.Unlock()
	require.Equal(t, []string{"worker", "app"}, order)
	require.NoError(t, workerStop)
	require.False(t, synthetic.Started())
}
