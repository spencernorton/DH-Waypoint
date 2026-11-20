// Package searchcmd provides `dh-waypoint search bm25` — operator
// management for the opt-in ParadeDB pg_search BM25 execution path. Stock
// PostgreSQL deployments need none of this: enable installs the generated
// columns and BM25 index on a ParadeDB 0.26.6 (PG18) database, verify
// confirms top-K execution plans, and disable removes them again.
package searchcmd

import (
	"fmt"
	"strings"

	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
	"gorm.io/gorm"
)

type Params struct {
	fx.In
	Dao lazy.Lazy[*dao.Query]
}

type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func (p Params) db(ctx *cli.Context) (*gorm.DB, error) {
	if p.Dao == nil {
		return nil, fmt.Errorf("database is not configured")
	}
	q, err := p.Dao.Get()
	if err != nil {
		return nil, err
	}
	return q.TorrentContent.WithContext(ctx.Context).UnderlyingDB(), nil
}

func writeStatus(ctx *cli.Context, status search.BM25Status) {
	w := ctx.App.Writer
	fmt.Fprintf(w, "mode (SEARCH_BM25_MODE): %s\n", status.Mode)
	fmt.Fprintf(w, "pg_search available: %v\n", status.ExtensionAvailable)
	installed := fmt.Sprintf("%v", status.ExtensionInstalled)
	if status.ExtensionVersion != "" {
		installed += " (version " + status.ExtensionVersion + ")"
	}
	fmt.Fprintf(w, "pg_search installed: %s\n", installed)
	fmt.Fprintf(w, "search_text column: %v (generated: %v)\n", status.SearchTextColumn, status.SearchTextGenerated)
	fmt.Fprintf(w, "ep_text column: %v (generated: %v)\n", status.EpisodeTextColumn, status.EpisodeTextGenerated)
	fmt.Fprintf(w, "torrent_contents_bm25_idx: exists=%v valid=%v\n", status.IndexExists, status.IndexValid)
	fmt.Fprintf(w, "active: %v\n", status.Active)
}

func New(p Params) (Result, error) {
	bm25 := &cli.Command{
		Name:  "bm25",
		Usage: "Manage opt-in ParadeDB BM25 search (pg_search)",
		Subcommands: []*cli.Command{
			{
				Name:  "status",
				Usage: "Show ParadeDB BM25 installation and activation state",
				Action: func(ctx *cli.Context) error {
					db, err := p.db(ctx)
					if err != nil {
						return cli.Exit(err.Error(), 1)
					}
					status, err := search.ParadeDBBM25Status(ctx.Context, db)
					if err != nil {
						return cli.Exit(err.Error(), 1)
					}
					writeStatus(ctx, status)
					return nil
				},
			},
			{
				Name:  "enable",
				Usage: "Install pg_search, generated columns, and the BM25 index",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "rebuild", Usage: "drop and recreate torrent_contents_bm25_idx to include newly supported filter fields"},
				},
				Action: func(ctx *cli.Context) error {
					db, err := p.db(ctx)
					if err != nil {
						return cli.Exit(err.Error(), 1)
					}
					enableFn := search.EnableParadeDBBM25
					if ctx.Bool("rebuild") {
						enableFn = search.RebuildParadeDBBM25
					}
					if err := enableFn(ctx.Context, db); err != nil {
						return cli.Exit(err.Error(), 1)
					}
					status, err := search.ParadeDBBM25Status(ctx.Context, db)
					if err != nil {
						return cli.Exit(err.Error(), 1)
					}
					writeStatus(ctx, status)
					fmt.Fprintln(ctx.App.Writer, "enabled: torrent_contents_bm25_idx is valid; run `dh-waypoint search bm25 verify` to confirm top-K plans, then restart dh-waypoint so running processes pick up BM25 on fresh connections")
					return nil
				},
			},
			{
				Name:  "verify",
				Usage: "Verify representative queries use ParadeDB top-K scans",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "verbose", Usage: "print the full EXPLAIN (COSTS OFF) plan for each check"},
				},
				Action: func(ctx *cli.Context) error {
					db, err := p.db(ctx)
					if err != nil {
						return cli.Exit(err.Error(), 1)
					}
					checks, err := search.VerifyParadeDBBM25Plan(ctx.Context, db)
					for _, check := range checks {
						label := "ok"
						if !check.UsesBM25Scan || !check.UsesTopK {
							label = "FAIL"
						}
						fmt.Fprintf(ctx.App.Writer, "%s: %s (bm25_scan=%v top_k=%v)\n", label, check.Name, check.UsesBM25Scan, check.UsesTopK)
						if ctx.Bool("verbose") {
							for _, line := range strings.Split(check.Plan, "\n") {
								fmt.Fprintf(ctx.App.Writer, "  %s\n", line)
							}
						}
					}
					if err != nil {
						return cli.Exit(err.Error(), 1)
					}
					fmt.Fprintf(ctx.App.Writer, "verified: %d BM25 plan check(s) use a ParadeDB top-K scan\n", len(checks))
					return nil
				},
			},
			{
				Name:  "disable",
				Usage: "Drop the BM25 index and generated search_text/ep_text columns",
				Action: func(ctx *cli.Context) error {
					db, err := p.db(ctx)
					if err != nil {
						return cli.Exit(err.Error(), 1)
					}
					if err := search.DisableParadeDBBM25(ctx.Context, db); err != nil {
						return cli.Exit(err.Error(), 1)
					}
					status, err := search.ParadeDBBM25Status(ctx.Context, db)
					if err != nil {
						return cli.Exit(err.Error(), 1)
					}
					writeStatus(ctx, status)
					fmt.Fprintln(ctx.App.Writer, "disabled: BM25 index and generated columns removed; the pg_search extension remains installed but unused; restart dh-waypoint so running processes drop cached plans for the removed columns")
					return nil
				},
			},
		},
	}

	return Result{Command: &cli.Command{
		Name:        "search",
		Usage:       "Search index management",
		Subcommands: []*cli.Command{bm25},
	}}, nil
}
