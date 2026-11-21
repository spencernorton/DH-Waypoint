// Package leanmodefx wires the opt-in LEAN MODE catalog curation extension.
package leanmodefx

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/config/configfx"
	"github.com/spencernorton/DH-Waypoint/internal/leanmode"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"lean_mode",
		configfx.NewConfigModule[leanmode.Config]("lean_mode", leanmode.NewDefaultConfig()),
		fx.Provide(
			leanmode.NewStore,
			leanmode.NewMetrics,
			leanmode.NewWorker,
		),
		fx.Provide(fx.Annotated{
			Group: "prometheus_collectors,flatten",
			Target: func(m *leanmode.Metrics) []prometheus.Collector {
				return m.Collectors()
			},
		}),
	)
}
