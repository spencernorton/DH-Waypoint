package processorfx

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/config/configfx"
	"github.com/spencernorton/DH-Waypoint/internal/processor"
	batchqueue "github.com/spencernorton/DH-Waypoint/internal/processor/batch/queue"
	processorqueue "github.com/spencernorton/DH-Waypoint/internal/processor/queue"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"processor",
		configfx.NewConfigModule[processor.Config]("processing", processor.NewDefaultConfig()),
		fx.Provide(
			processor.New,
			processorqueue.New,
			batchqueue.New,
			processor.NewDeleteMetrics,
			fx.Annotated{
				Group: "prometheus_collectors,flatten",
				Target: func(m *processor.DeleteMetrics) []prometheus.Collector {
					return m.Collectors()
				},
			},
		),
	)
}
