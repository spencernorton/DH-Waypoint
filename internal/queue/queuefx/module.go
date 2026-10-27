package queuefx

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/config/configfx"
	"github.com/spencernorton/DH-Waypoint/internal/queue"
	"github.com/spencernorton/DH-Waypoint/internal/queue/manager"
	queueprom "github.com/spencernorton/DH-Waypoint/internal/queue/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/queue/server"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"queue",
		fx.Provide(
			server.New,
			manager.New,
			queueprom.New,
		),
		// Queue purge worker (config section "queueclean", env
		// QUEUECLEAN_*). Opt-in: disabled by default, dry-run until
		// EnablePurge is set, so including it is always safe.
		configfx.NewConfigModule[queue.PurgeConfig]("queueclean", queue.NewPurgeDefaultConfig()),
		fx.Provide(
			queue.NewPurgeMetrics,
			queue.NewPurgeWorker,
		),
		fx.Provide(fx.Annotated{
			Group: "prometheus_collectors,flatten",
			Target: func(m *queue.PurgeMetrics) []prometheus.Collector {
				return m.Collectors()
			},
		}),
	)
}
