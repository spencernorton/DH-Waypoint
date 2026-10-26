package app

import (
	"github.com/spencernorton/DH-Waypoint/internal/app/appfx"
	appcli "github.com/spencernorton/DH-Waypoint/internal/app/cli"
	"github.com/spencernorton/DH-Waypoint/internal/logging/loggingfx"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

func New() *fx.App {
	return fx.New(
		appfx.New(),
		loggingfx.WithLogger(),
		fx.Invoke(func(
			logger *zap.SugaredLogger,
			_ *cli.App,
			_ appcli.AttachedHooks,
		) {
			logger.Debug("app invoked")
		}),
	)
}
