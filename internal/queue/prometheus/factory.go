package prometheus

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type Params struct {
	fx.In
	Query  lazy.Lazy[*dao.Query]
	Logger *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Collector prometheus.Collector `group:"prometheus_collectors"`
}

func New(p Params) Result {
	return Result{
		Collector: &queueMetricsCollector{
			query:  p.Query,
			logger: p.Logger.Named("queue_metrics_collector"),
		},
	}
}
