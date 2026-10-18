package devfx

import (
	"github.com/spencernorton/DH-Waypoint/internal/app/cli"
	"github.com/spencernorton/DH-Waypoint/internal/app/cli/args"
	"github.com/spencernorton/DH-Waypoint/internal/config/configfx"
	"github.com/spencernorton/DH-Waypoint/internal/database"
	"github.com/spencernorton/DH-Waypoint/internal/database/migrations"
	"github.com/spencernorton/DH-Waypoint/internal/database/postgres"
	"github.com/spencernorton/DH-Waypoint/internal/dev/app/cmd/gormcmd"
	"github.com/spencernorton/DH-Waypoint/internal/dev/app/cmd/migratecmd"
	"github.com/spencernorton/DH-Waypoint/internal/logging/loggingfx"
	"github.com/spencernorton/DH-Waypoint/internal/validation/validationfx"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"dev",
		configfx.NewConfigModule[postgres.Config]("postgres", postgres.NewDefaultConfig()),
		configfx.New(),
		loggingfx.New(),
		validationfx.New(),
		fx.Provide(args.New),
		fx.Provide(cli.New),
		fx.Provide(database.New),
		fx.Provide(migrations.New),
		fx.Provide(postgres.New),
		fx.Provide(gormcmd.New),
		fx.Provide(migratecmd.New),
	)
}
