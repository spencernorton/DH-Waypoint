// Package storagecmd exposes the compact-file-bundles backfill command.
package storagecmd

import (
	"encoding/json"
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
				},
				Action: func(ctx *cli.Context) error {
					pool, err := p.Pool.Get()
					if err != nil {
						return err
					}
					if ctx.Bool("status") {
						status, err := filebundles.Status(ctx.Context, pool)
						if err != nil {
							return err
						}
						return json.NewEncoder(ctx.App.Writer).Encode(status)
					}
					report, err := filebundles.Backfill(ctx.Context, pool, filebundles.BackfillOptions{
						BatchSize:   ctx.Int("batch-size"),
						Pause:       ctx.Duration("pause"),
						MaxBatches:  ctx.Int("max-batches"),
						VerifyOnly:  ctx.Bool("verify-only"),
						PruneLegacy: ctx.Bool("prune-legacy"),
						ResetCursor: ctx.Bool("reset-cursor"),
					})
					if err != nil {
						return err
					}
					return json.NewEncoder(ctx.App.Writer).Encode(report)
				},
			},
		},
	}}, nil
}
