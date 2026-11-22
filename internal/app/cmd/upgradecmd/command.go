// Package upgradecmd provides `dh-waypoint upgrade`: read-only pre-flight
// inspection (`--check`) and one-command backup-acknowledged automatic
// in-binary upgrade from compatible v5.12-v5.15 databases into v6.0.0.
package upgradecmd

import (
	"database/sql"
	"fmt"

	"github.com/spencernorton/DH-Waypoint/internal/database/migrations"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
)

type Params struct {
	fx.In
	SQLDB    lazy.Lazy[*sql.DB]
	Migrator lazy.Lazy[migrations.Migrator]
}

type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func New(p Params) (Result, error) {
	cmd := &cli.Command{
		Name:  "upgrade",
		Usage: "Inspect v5 -> v6 upgrade readiness or run automatic backup-acknowledged v6 upgrade",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "check",
				Usage: "read-only pre-flight inspection (writes nothing to PostgreSQL)",
			},
			&cli.StringFlag{
				Name:  "confirm-backup",
				Usage: "12-char DB fingerprint confirming a verified PostgreSQL backup before v5 -> v6 upgrade",
			},
		},
		Action: func(ctx *cli.Context) error {
			out := ctx.App.Writer
			db, err := p.SQLDB.Get()
			if err != nil {
				return err
			}
			if ctx.Bool("check") {
				rep, err := migrations.InspectV6Readiness(ctx.Context, db)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "PostgreSQL:   %s (major=%d)\n", rep.PostgresVersion, rep.PostgresMajor)
				fmt.Fprintf(out, "Database:     %s\n", rep.DatabaseName)
				fmt.Fprintf(out, "Goose schema: %d (schema_major=%d, set=%v)\n", rep.GooseVersion, rep.SchemaMajor, rep.HasSchemaMajor)
				fmt.Fprintf(out, "Fingerprint:  %s\n", rep.Fingerprint)
				migrations.WriteReadinessDetails(out, rep, "")
				if !rep.Ready {
					return cli.Exit("upgrade pre-flight found blocking issues", migrations.ExitCodeIncompatibleSchema)
				}
				return nil
			}

			m, err := p.Migrator.Get()
			if err != nil {
				return err
			}
			outcome, err := m.UpWithConfirm(ctx.Context, ctx.String("confirm-backup"))
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Upgrade complete (case=%s, schema_major=%d). Subsequent startups require no confirmation flag.\n",
				outcome.Case, migrations.CurrentSchemaMajor)
			return nil
		},
	}
	return Result{Command: cmd}, nil
}
