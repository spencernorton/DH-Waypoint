// Package leancmd exposes `dh-waypoint lean` subcommands for validating profiles,
// running reconciliation/pruning, explaining title decisions, inspecting status,
// and resetting tombstones.
package leancmd

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/leanmode"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
)

type Params struct {
	fx.In
	Store *leanmode.Store
}

type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func New(p Params) (Result, error) {
	return Result{Command: &cli.Command{
		Name:  "lean",
		Usage: "Opt-in LEAN MODE catalog curation commands",
		Subcommands: []*cli.Command{
			{
				Name:      "validate",
				Usage:     "Strictly validate a LEAN MODE YAML profile and print its canonical config digest",
				ArgsUsage: "<profile.yml>",
				Action: func(ctx *cli.Context) error {
					if ctx.NArg() != 1 {
						return cli.Exit("usage: dh-waypoint lean validate <profile.yml>", 1)
					}
					prof, err := leanmode.LoadProfileFile(ctx.Args().First())
					if err != nil {
						return cli.Exit(err.Error(), 1)
					}
					return json.NewEncoder(ctx.App.Writer).Encode(map[string]any{
						"ok":                 true,
						"id":                 prof.ID,
						"mode":               prof.Mode,
						"enabled":            prof.Enabled,
						"config_digest":      prof.ConfigDigest(),
						"season_pack_policy": prof.Scope.SeasonPackPolicy,
						"keep_runner_up":     prof.Resilience.KeepRunnerUp,
					})
				},
			},
			{
				Name:  "reconcile",
				Usage: "Reconcile movie and TV slots against a LEAN MODE YAML profile",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "config", Required: true, Usage: "Path to LEAN MODE YAML profile"},
					&cli.StringFlag{Name: "mode", Usage: "Override profile mode (observe, serve, prune)"},
					&cli.BoolFlag{Name: "confirm-destructive", Usage: "Explicitly confirm physical deletion when mode=prune"},
					&cli.IntFlag{Name: "batch-size", Value: 200, Usage: "Titles per reconciliation batch"},
					&cli.IntFlag{Name: "max-titles", Value: 0, Usage: "Cap titles evaluated (0 = full sweep)"},
					&cli.BoolFlag{Name: "reset-cursor", Usage: "Restart incremental sweep from beginning"},
				},
				Action: func(ctx *cli.Context) error {
					prof, err := leanmode.LoadProfileFile(ctx.String("config"))
					if err != nil {
						return err
					}
					if m := ctx.String("mode"); m != "" {
						prof.Mode = m
					}
					if ctx.Bool("confirm-destructive") {
						prof.Prune.ConfirmDestructive = true
					}
					if err := prof.Validate(); err != nil {
						return err
					}
					report, err := p.Store.Reconcile(ctx.Context, prof, leanmode.ReconcileOptions{
						TitleBatchSize: ctx.Int("batch-size"),
						MaxTitles:      ctx.Int("max-titles"),
						ResetCursor:    ctx.Bool("reset-cursor"),
						Now:            time.Now().UTC(),
					})
					if err != nil {
						return err
					}
					return json.NewEncoder(ctx.App.Writer).Encode(report)
				},
			},
			{
				Name:  "explain",
				Usage: "Explain the deterministic three-tier ranking for one movie or TV show",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "config", Required: true, Usage: "Path to LEAN MODE YAML profile"},
					&cli.StringFlag{Name: "type", Required: true, Usage: "movie or tv_show"},
					&cli.StringFlag{Name: "source", Value: "tmdb", Usage: "content_source (e.g. tmdb, imdb)"},
					&cli.StringFlag{Name: "id", Required: true, Usage: "content_id"},
				},
				Action: func(ctx *cli.Context) error {
					prof, err := leanmode.LoadProfileFile(ctx.String("config"))
					if err != nil {
						return err
					}
					td, err := p.Store.ExplainTitle(ctx.Context, prof, ctx.String("type"), ctx.String("source"), ctx.String("id"), time.Now().UTC())
					if err != nil {
						return err
					}
					type slotExplain struct {
						SlotKey          string                `json:"slot_key"`
						Kind             leanmode.SlotKind     `json:"kind"`
						HysteresisHeld   bool                  `json:"hysteresis_held"`
						NeedsSlotRevival bool                  `json:"needs_slot_revival"`
						Candidates       []leanmode.ExplainRow `json:"candidates"`
					}
					slots := make([]slotExplain, 0, len(td.Slots))
					for _, sd := range td.Slots {
						slots = append(slots, slotExplain{
							SlotKey:          sd.SlotKey,
							Kind:             sd.Kind,
							HysteresisHeld:   sd.HysteresisHeld,
							NeedsSlotRevival: sd.NeedsSlotRevival,
							Candidates:       leanmode.ExplainDecision(sd),
						})
					}
					return json.NewEncoder(ctx.App.Writer).Encode(map[string]any{
						"profile_id":     prof.ID,
						"config_digest":  prof.ConfigDigest(),
						"content_type":   td.ContentType,
						"content_source": td.ContentSource,
						"content_id":     td.ContentID,
						"slots":          slots,
					})
				},
			},
			{
				Name:  "status",
				Usage: "Report persisted LEAN MODE profile counts, runner-ups, and active tombstones",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "profile", Usage: "Filter by profile ID"},
				},
				Action: func(ctx *cli.Context) error {
					statuses, err := p.Store.Status(ctx.Context, ctx.String("profile"))
					if err != nil {
						return err
					}
					return json.NewEncoder(ctx.App.Writer).Encode(statuses)
				},
			},
			{
				Name:  "clear-tombstones",
				Usage: "Clear LEAN MODE tombstones for a profile or a specific slot so crawler/importer can re-admit hashes",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "profile", Required: true, Usage: "Profile ID"},
					&cli.StringFlag{Name: "slot", Usage: "Optional slot key to revive"},
				},
				Action: func(ctx *cli.Context) error {
					n, err := p.Store.ClearTombstones(ctx.Context, ctx.String("profile"), ctx.String("slot"))
					if err != nil {
						return err
					}
					fmt.Fprintf(ctx.App.Writer, "cleared %d tombstones\n", n)
					return nil
				},
			},
		},
	}}, nil
}
