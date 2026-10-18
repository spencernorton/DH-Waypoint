package metricsfx

import (
	"github.com/spencernorton/DH-Waypoint/internal/metrics/queuemetrics"
	"github.com/spencernorton/DH-Waypoint/internal/metrics/torrentmetrics"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"queue",
		fx.Provide(
			queuemetrics.New,
			torrentmetrics.New,
		),
	)
}
