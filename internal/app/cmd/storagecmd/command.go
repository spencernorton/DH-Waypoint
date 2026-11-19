// Package storagecmd exposes the compact-file-bundles backfill command.
package storagecmd

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencernorton/DH-Waypoint/internal/database/filebundles"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
)

// Params is injected by fx.
type Params struct {
	fx.In
	Pool lazy.Lazy[*pgxpool.Pool]
}

// Result registers the storage command group.
type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

// New builds the storage command.
func New(p Params) (Result, error) {
	return Result{Command: &cli.Command{
		Name:  "storage",
		Usage: "Storage maintenance commands",
		Subcommands: []*cli.Command{
			{
				Name:  "compact-files",
				Usage: "Backfill, verify or report compact torrent file bundles",
				Flags: []cli.Flag{
					&cli.IntFlag{Name: "batch-size", Value: 2000, Usage: "hashes per batch"},
					&cli.DurationFlag{Name: "pause", Value: 25 * time.Millisecond, Usage: "pause between batches"},
					&cli.IntFlag{Name: "max-batches", Usage: "stop after this many batches (0 = no limit)"},
					&cli.BoolFlag{Name: "verify-only", Usage: "only verify bundle/legacy parity"},
					&cli.BoolFlag{Name: "prune-legacy", Usage: "delete legacy torrent_files rows after verified migration"},
					&cli.BoolFlag{Name: "reset-cursor", Usage: "delete the persisted backfill cursor before running"},
					&cli.BoolFlag{Name: "status", Usage: "print legacy/bundle counts, sizes and cursor instead of backfilling"},
					&cli.BoolFlag{Name: "fast", Usage: "with --status, use O(1) pg_class.reltuples estimates without table scans (sets unmigrated_hashes=-1)"},
					&cli.DurationFlag{Name: "progress-interval", Value: 5 * time.Second, Usage: "interval for NDJSON progress updates on stderr (0 disables)"},
				},
				Action: func(ctx *cli.Context) error {
					pool, err := p.Pool.Get()
					if err != nil {
						return err
					}
					if ctx.Bool("status") {
						var status filebundles.StatusReport
						if ctx.Bool("fast") {
							status, err = filebundles.StatusFast(ctx.Context, pool)
						} else {
							status, err = filebundles.Status(ctx.Context, pool)
						}
						if err != nil {
							return err
						}
						return json.NewEncoder(ctx.App.Writer).Encode(status)
					}

					// First SIGINT/SIGTERM requests a clean checkpoint after the current
					// batch commits; a second signal cancels the context immediately.
					runCtx, cancel := context.WithCancel(ctx.Context)
					defer cancel()
					stopCh := make(chan struct{})
					doneCh := make(chan struct{})
					sigCh := make(chan os.Signal, 2)
					signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
					defer func() {
						signal.Stop(sigCh)
						close(doneCh)
					}()
					go func() {
						stopped := false
						for {
							select {
							case <-doneCh:
								return
							case <-sigCh:
								if !stopped {
									stopped = true
									close(stopCh)
								} else {
									cancel()
									return
								}
							}
						}
					}()

					opts := filebundles.BackfillOptions{
						BatchSize:   ctx.Int("batch-size"),
						Pause:       ctx.Duration("pause"),
						MaxBatches:  ctx.Int("max-batches"),
						VerifyOnly:  ctx.Bool("verify-only"),
						PruneLegacy: ctx.Bool("prune-legacy"),
						ResetCursor: ctx.Bool("reset-cursor"),
						Stop:        stopCh,
					}
					if interval := ctx.Duration("progress-interval"); interval > 0 && ctx.App.ErrWriter != nil {
						enc := json.NewEncoder(ctx.App.ErrWriter)
						opts.ProgressInterval = interval
						opts.Progress = func(p filebundles.BackfillProgress) {
							_ = enc.Encode(p)
						}
					}
					report, err := filebundles.Backfill(runCtx, pool, opts)
					if err != nil {
						return err
					}
					return json.NewEncoder(ctx.App.Writer).Encode(report)
				},
			},
		},
	}}, nil
}
