package metainforequester

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
	"github.com/spencernorton/DH-Waypoint/internal/telemetry/dualemit"
)

type prometheusCollector struct {
	requester             Requester
	requestDuration       *dualemit.Histogram
	requestSuccessTotal   *dualemit.Counter
	requestErrorTotal     *dualemit.Counter
	requestErrorsByReason *dualemit.CounterVec
	requestConcurrency    *dualemit.Gauge
}

const (
	namespace = "dh_waypoint"
	subsystem = "meta_info_requester"
)

func newPrometheusCollector(requester Requester) *prometheusCollector {
	return &prometheusCollector{
		requester: requester,
		requestDuration: dualemit.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "duration_seconds",
			Help:      "Duration of successful meta info requests in seconds.",
			Buckets:   prometheus.DefBuckets,
		}),
		requestSuccessTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "success_total",
			Help:      "Total number of successful meta info requests.",
		}),
		requestErrorTotal: dualemit.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "error_total",
			Help:      "Total number of failed meta info requests.",
		}),
		requestErrorsByReason: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "errors_by_reason_total",
			Help:      "Failed meta info requests partitioned by low-cardinality transport and protocol failure stage.",
		}, []string{"reason"}),
		requestConcurrency: dualemit.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "concurrency",
			Help:      "Number of concurrent meta info requests.",
		}),
	}
}

func (l prometheusCollector) Request(ctx context.Context, infoHash protocol.ID, addr netip.AddrPort) (Response, error) {
	l.requestConcurrency.Inc()
	defer l.requestConcurrency.Dec()

	start := time.Now()
	resp, err := l.requester.Request(ctx, infoHash, addr)

	if err == nil {
		l.requestDuration.Observe(time.Since(start).Seconds())
		l.requestSuccessTotal.Inc()
	} else {
		l.requestErrorTotal.Inc()
		l.requestErrorsByReason.WithLabelValues(classifyRequesterError(err)).Inc()
	}

	return resp, err
}

func classifyRequesterError(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "dial_refused"
	}
	if errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return "dial_unreachable"
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return "connection_reset"
	}
	if errors.Is(err, metainfo.ErrWrongInfoHash) {
		return "wrong_infohash"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "eof"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "non-public address"):
		return "non_public_addr"
	case strings.Contains(msg, "does not support the extension protocol"):
		return "no_extension_support"
	case strings.Contains(msg, "infohash mismatch"):
		return "infohash_mismatch"
	case strings.Contains(msg, "handshake"):
		return "bt_handshake"
	case strings.Contains(msg, "metadata too big"), strings.Contains(msg, "ut_metadata is not an uint8"), strings.Contains(msg, "extension handshake"):
		return "ex_handshake"
	case strings.Contains(msg, "rejected metadata piece"):
		return "piece_rejected"
	case strings.Contains(msg, "ut_metadata"), strings.Contains(msg, "longer than max allowed"):
		return "protocol_error"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return "dial_error"
	}
	return "other"
}
