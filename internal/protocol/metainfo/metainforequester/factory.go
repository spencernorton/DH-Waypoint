package metainforequester

import (
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/concurrency"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo/peerrep"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/time/rate"
)

type Params struct {
	fx.In
	Config        Config
	PeerRepConfig peerrep.Config
	Logger        *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Requester           Requester
	RequestDuration     prometheus.Collector   `group:"prometheus_collectors"`
	RequestSuccessTotal prometheus.Collector   `group:"prometheus_collectors"`
	RequestErrorTotal   prometheus.Collector   `group:"prometheus_collectors"`
	RequestConcurrency  prometheus.Collector   `group:"prometheus_collectors"`
	PeerRepCollectors   []prometheus.Collector `group:"prometheus_collectors,flatten"`
}

// refuseNonPublic keeps the dialer away from addresses no DHT peer can have,
// such as the operator's own machine and network.
func refuseNonPublic(_, address string, _ syscall.RawConn) error {
	addr, err := netip.ParseAddrPort(address)
	if err != nil || !protocol.IsPublicAddr(addr.Addr()) {
		return fmt.Errorf("refusing to connect to non-public address %s", address)
	}

	return nil
}

func New(p Params) Result {
	dialer := &net.Dialer{
		Timeout:   3 * time.Second,
		KeepAlive: -1,
	}
	if !p.Config.AllowPrivateAddrs {
		dialer.Control = refuseNonPublic
	}

	collector := newPrometheusCollector(requester{
		clientID: protocol.RandomPeerID(),
		timeout:  p.Config.RequestTimeout,
		dialer:   dialer,
	})

	// Per-peer reputation cache. Defaults: Enabled=false, so the
	// wrapper is a no-op until the operator opts in via env. When
	// enabled but Enforce=false (shadow), every Decide() returns
	// Allow=true while the metrics light up — that's the
	// counterfactual measurement primitive.
	peerRepStore := peerrep.NewStore(p.PeerRepConfig, nil)
	peerRepMet := newPeerRepMetrics()

	// Chain (outer → inner):
	//   peerRepWrapper           — skip dead peers (cheap; before the
	//                              rate-limit token cost)
	//   requestLimiter           — per-IP outbound rate (0.5/s, burst 4)
	//   requestLogger            — sampled log of inner outcomes
	//   prometheusCollector      — concurrency / duration / success / err
	//   requester (real BEP-9)
	chained := newPeerRepWrapper(
		requestLimiter{
			requester: requestLogger{
				requester: collector,
				logger: p.Logger.WithOptions(zap.WrapCore(func(core zapcore.Core) zapcore.Core {
					return zapcore.NewSamplerWithOptions(core, time.Minute, 10, 0)
				})).Named("meta_info_requester"),
			},
			limiter: concurrency.NewKeyedLimiter(rate.Every(time.Second/2), 4, 1000, time.Second*20),
		},
		peerRepStore,
		peerRepMet,
	)

	return Result{
		Requester:           chained,
		RequestDuration:     collector.requestDuration,
		RequestSuccessTotal: collector.requestSuccessTotal,
		RequestErrorTotal:   collector.requestErrorTotal,
		RequestConcurrency:  collector.requestConcurrency,
		PeerRepCollectors:   peerRepMet.collectors(),
	}
}
