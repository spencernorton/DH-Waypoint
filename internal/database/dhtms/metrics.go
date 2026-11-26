package dhtms

import "github.com/prometheus/client_golang/prometheus"

// Metrics exposes operational telemetry for DH-TMS sync and read serving.
type Metrics struct {
	readsTotal      *prometheus.CounterVec
	syncTotal       *prometheus.CounterVec
	outboxApplied   prometheus.Counter
	generationGauge prometheus.Gauge
	eligibleDocs    *prometheus.GaugeVec
}

// NewMetrics constructs Prometheus collectors for DH-TMS.
func NewMetrics() *Metrics {
	return &Metrics{
		readsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dh_waypoint_dhtms_reads_total",
			Help: "Read requests routed to DH-TMS by surface and outcome (hit, fallback_lease, fallback_error, bypass).",
		}, []string{"surface", "outcome"}),
		syncTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dh_waypoint_dhtms_sync_total",
			Help: "DH-TMS sync cycles by kind (full_build, incremental, lease) and outcome (ok, error).",
		}, []string{"kind", "outcome"}),
		outboxApplied: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "dh_waypoint_dhtms_outbox_applied_total",
			Help: "Total dhtms_outbox rows processed and cleared by the sync worker.",
		}),
		generationGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dh_waypoint_dhtms_generation_id",
			Help: "Active generation_id reported by the co-located dh-tmsd sidecar.",
		}),
		eligibleDocs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dh_waypoint_dhtms_eligible_documents",
			Help: "Documents in each policy tier at the last healthy leased sidecar observation.",
		}, []string{"tier"}),
	}
}

// Collectors returns the underlying Prometheus collectors for registration.
func (m *Metrics) Collectors() []prometheus.Collector {
	if m == nil {
		return nil
	}
	return []prometheus.Collector{
		m.readsTotal,
		m.syncTotal,
		m.outboxApplied,
		m.generationGauge,
		m.eligibleDocs,
	}
}

// RecordRead increments dh_waypoint_dhtms_reads_total.
func (m *Metrics) RecordRead(surface, outcome string) {
	if m == nil {
		return
	}
	m.readsTotal.WithLabelValues(surface, outcome).Inc()
}

// RecordSync increments dh_waypoint_dhtms_sync_total.
func (m *Metrics) RecordSync(kind, outcome string) {
	if m == nil {
		return
	}
	m.syncTotal.WithLabelValues(kind, outcome).Inc()
}

// AddOutboxApplied increments dh_waypoint_dhtms_outbox_applied_total.
func (m *Metrics) AddOutboxApplied(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.outboxApplied.Add(float64(n))
}

// SetGeneration updates dh_waypoint_dhtms_generation_id.
func (m *Metrics) SetGeneration(gen uint64) {
	if m == nil {
		return
	}
	m.generationGauge.Set(float64(gen))
}

func (m *Metrics) observeEligibility(health HealthResponse) {
	if m == nil {
		return
	}
	if health.Status != "ok" || !health.LeaseActive {
		m.eligibleDocs.Reset()
		return
	}
	m.eligibleDocs.WithLabelValues("base").Set(float64(health.BaseEligible))
	m.eligibleDocs.WithLabelValues("publication").Set(float64(health.PubEligible))
	m.eligibleDocs.WithLabelValues("payload").Set(float64(health.PayloadEligible))
}
