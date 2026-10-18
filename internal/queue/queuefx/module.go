package queuefx

import (
	"github.com/spencernorton/DH-Waypoint/internal/queue/manager"
	"github.com/spencernorton/DH-Waypoint/internal/queue/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/queue/server"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"queue",
		fx.Provide(
			server.New,
			manager.New,
			prometheus.New,
		),
	)
}
