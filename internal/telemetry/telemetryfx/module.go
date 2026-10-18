package telemetryfx

import (
	"github.com/spencernorton/DH-Waypoint/internal/telemetry/httpserver"
	"github.com/spencernorton/DH-Waypoint/internal/telemetry/prometheus"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"telemetry",
		fx.Provide(
			httpserver.New,
			prometheus.New,
		),
	)
}
