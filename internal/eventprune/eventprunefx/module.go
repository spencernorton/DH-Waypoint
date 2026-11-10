// Package eventprunefx wires the eventprune worker into the app.
package eventprunefx

import (
	"github.com/spencernorton/DH-Waypoint/internal/config/configfx"
	"github.com/spencernorton/DH-Waypoint/internal/eventprune"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"eventprune",
		configfx.NewConfigModule[eventprune.Config]("eventprune", eventprune.NewDefaultConfig()),
		fx.Provide(eventprune.New),
	)
}
