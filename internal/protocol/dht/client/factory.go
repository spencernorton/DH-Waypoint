package client

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht/server"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type Params struct {
	fx.In
	Identity lazy.Lazy[protocol.NodeIdentity]
	Server   lazy.Lazy[server.Server]
	Logger   *zap.SugaredLogger
}

type Result struct {
	fx.Out

	Client lazy.Lazy[Client]

	// Outbound RPC metrics. Kept as a distinct `dh_waypoint_dht_client_*`
	// namespace from the server-side `dh_waypoint_dht_server_*` so a
	// reader can tell direction from the metric name alone. Registered
	// via the shared prometheus_collectors fx group.
	RequestDuration    prometheus.Collector `group:"prometheus_collectors"`
	RequestSuccess     prometheus.Collector `group:"prometheus_collectors"`
	RequestError       prometheus.Collector `group:"prometheus_collectors"`
	RequestConcurrency prometheus.Collector `group:"prometheus_collectors"`
}

func New(p Params) Result {
	// Collectors are allocated up front so their registration with
	// Prometheus happens exactly once, regardless of how many times
	// the lazy client is materialized. The wrapper closes over the
	// same collector instance the registry holds.
	collector := newPrometheusCollector()

	return Result{
		Client: lazy.New(func() (Client, error) {
			s, err := p.Server.Get()
			if err != nil {
				return nil, err
			}
			identity, err := p.Identity.Get()
			if err != nil {
				return nil, err
			}

			adapter := serverAdapter{
				nodeID: identity.ID,
				server: s,
			}

			logged := clientLogger{
				client: adapter,
				// we make way too many queries to usefully log
				// everything, but having a sample is helpful:
				logger: p.Logger.WithOptions(zap.WrapCore(func(core zapcore.Core) zapcore.Core {
					return zapcore.NewSamplerWithOptions(core, time.Minute, 10, 0)
				})).Named("dht_client"),
			}

			// Wrap order matters: metrics record what the rest of
			// the stack actually observed, so prometheus sits on the
			// outside. If we instead wrapped prometheus inside
			// `clientLogger`, retries or transforms in the logger
			// wrapper could hide errors from the metrics.
			return prometheusClientWrapper{
				prometheusCollector: collector,
				inner:               logged,
			}, nil
		}),
		RequestDuration:    collector.requestDuration,
		RequestSuccess:     collector.requestSuccess,
		RequestError:       collector.requestError,
		RequestConcurrency: collector.requestConcurrency,
	}
}
