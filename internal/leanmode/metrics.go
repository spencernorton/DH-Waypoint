package leanmode

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/telemetry/dualemit"
)

// Metrics holds Prometheus instrumentation for LEAN MODE reconciliation, pruning,
// and tombstone skips.
type Metrics struct {
	cyclesTotal       *dualemit.CounterVec
	cycleDuration     *dualemit.Histogram
	slotsEvaluated    *dualemit.CounterVec
	winnersGauge      *dualemit.GaugeVec
	runnerUpsGauge    *dualemit.GaugeVec
	disqualifiedTotal *dualemit.CounterVec
	wouldPruneTotal   *dualemit.CounterVec
	prunedTotal       *dualemit.CounterVec
	revivedSlotsTotal *dualemit.CounterVec
	tombstoneSkips    *dualemit.CounterVec
	cycleErrorsTotal  *dualemit.CounterVec
}

func NewMetrics() *Metrics {
	return &Metrics{
		cyclesTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dh_waypoint",
			Subsystem: "lean_mode",
			Name:      "cycles_total",
			Help:      "Completed LEAN MODE reconciliation cycles by profile and mode.",
		}, []string{"profile", "mode"}),
		cycleDuration: dualemit.NewHistogram(prometheus.HistogramOpts{
			Namespace: "dh_waypoint",
			Subsystem: "lean_mode",
			Name:      "cycle_duration_seconds",
			Help:      "Duration of LEAN MODE reconciliation cycles.",
			Buckets:   []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 180, 600},
		}),
		slotsEvaluated: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dh_waypoint",
			Subsystem: "lean_mode",
			Name:      "slots_evaluated_total",
			Help:      "Movie and TV episode/pack slots evaluated by LEAN MODE.",
		}, []string{"profile"}),
		winnersGauge: dualemit.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "dh_waypoint",
			Subsystem: "lean_mode",
			Name:      "winners_current",
			Help:      "Current count of rank-1 winners in lean_winners.",
		}, []string{"profile"}),
		runnerUpsGauge: dualemit.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "dh_waypoint",
			Subsystem: "lean_mode",
			Name:      "runner_ups_current",
			Help:      "Current count of warm standby runner-ups (rank > 1) in lean_winners.",
		}, []string{"profile"}),
		disqualifiedTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dh_waypoint",
			Subsystem: "lean_mode",
			Name:      "disqualified_total",
			Help:      "Candidates disqualified by Tier 1 required rules.",
		}, []string{"profile", "reason"}),
		wouldPruneTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dh_waypoint",
			Subsystem: "lean_mode",
			Name:      "would_prune_total",
			Help:      "Dominated or disqualified candidates eligible for pruning in observe/serve dry-run.",
		}, []string{"profile"}),
		prunedTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dh_waypoint",
			Subsystem: "lean_mode",
			Name:      "pruned_total",
			Help:      "Torrents physically pruned and tombstoned under opt-in prune mode.",
		}, []string{"profile"}),
		revivedSlotsTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dh_waypoint",
			Subsystem: "lean_mode",
			Name:      "revived_slots_total",
			Help:      "Slots whose tombstones were cleared after incumbent + runner-up death.",
		}, []string{"profile"}),
		tombstoneSkips: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dh_waypoint",
			Subsystem: "lean_mode",
			Name:      "tombstone_skips_total",
			Help:      "Infohashes skipped by crawler or importer because of an active LEAN MODE tombstone.",
		}, []string{"reader"}),
		cycleErrorsTotal: dualemit.NewCounterVec(prometheus.CounterOpts{
			Namespace: "dh_waypoint",
			Subsystem: "lean_mode",
			Name:      "errors_total",
			Help:      "LEAN MODE operational errors by stage.",
		}, []string{"stage"}),
	}
}

func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.cyclesTotal,
		m.cycleDuration,
		m.slotsEvaluated,
		m.winnersGauge,
		m.runnerUpsGauge,
		m.disqualifiedTotal,
		m.wouldPruneTotal,
		m.prunedTotal,
		m.revivedSlotsTotal,
		m.tombstoneSkips,
		m.cycleErrorsTotal,
	}
}

func (m *Metrics) TombstoneSkipped(reader string, n int) {
	if m == nil || n <= 0 {
		return
	}
	m.tombstoneSkips.WithLabelValues(reader).Add(float64(n))
}
