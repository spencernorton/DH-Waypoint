package csamblocklist

import "github.com/prometheus/client_golang/prometheus"

// Metrics is the Prometheus surface for the CSAM blocklist. All
// counters and gauges share the dh_waypoint_csam_blocklist_* prefix.
//
// Convention: feed labels are the URL host+path (no scheme, no query)
// to keep cardinality bounded. Reasons are stable strings.
type Metrics struct {
	// PrefetchBlocksTotal counts hashes rejected pre-fetch by this
	// blocklist. Mirrors blocking_manager.filter outcome but for
	// community-feed-known hashes specifically.
	PrefetchBlocksTotal prometheus.Counter

	// LookupsTotal counts every IsBlocked / Filter call (per hash).
	// Useful as a sanity gauge that the hot path is wired.
	LookupsTotal prometheus.Counter

	// FeedRefreshTotal labels by feed and outcome (success / error).
	FeedRefreshTotal *prometheus.CounterVec

	// FeedRefreshDurationSeconds labels by feed.
	FeedRefreshDurationSeconds *prometheus.HistogramVec

	// FeedEntriesGauge labels by feed; current entry count. The label
	// "local" is this instance's own observations.
	FeedEntriesGauge *prometheus.GaugeVec

	// KeywordMatchesTotal counts torrents dropped before they were stored
	// because they matched the built-in banned-keyword list, by source
	// (crawler / import).
	KeywordMatchesTotal *prometheus.CounterVec

	// StoredMatchesTotal counts stored torrents that matched the built-in
	// banned-keyword list, by source and by what was done with them.
	StoredMatchesTotal *prometheus.CounterVec

	// SweepRowsScanned, SweepMatches and SweepDone show the saved state of
	// the sweep of stored rows: the whole pass, across restarts.
	SweepRowsScanned *prometheus.GaugeVec
	SweepMatches     *prometheus.GaugeVec
	SweepDone        prometheus.Gauge
	// SweepBatchErrorsTotal tells a sweep that waits on a failing batch from
	// one that is only slow.
	SweepBatchErrorsTotal prometheus.Counter

	// EntriesGauge is the union across all feeds.
	EntriesGauge prometheus.Gauge

	// ExportTotal labels by outcome (local_ok / local_err /
	// upstream_ok / upstream_err / skipped_no_match).
	ExportTotal *prometheus.CounterVec
}

// NewMetrics constructs the metric set. Call Collectors() to get the
// list to register with Prometheus.
func NewMetrics() *Metrics {
	const (
		ns  = "dh_waypoint"
		sub = "csam_blocklist"
	)
	return &Metrics{
		PrefetchBlocksTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Subsystem: sub, Name: "prefetch_blocks_total",
			Help: "Infohashes rejected pre-fetch by the CSAM blocklist (community-feed-known double-hashes).",
		}),
		LookupsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Subsystem: sub, Name: "lookups_total",
			Help: "Total IsBlocked / Filter lookups (per hash).",
		}),
		FeedRefreshTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Subsystem: sub, Name: "feed_refresh_total",
			Help: "Per-feed refresh attempts. outcome=success|error.",
		}, []string{"feed", "outcome"}),
		FeedRefreshDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: ns, Subsystem: sub, Name: "feed_refresh_duration_seconds",
			Help:    "Per-feed refresh wallclock duration.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 10), // 50ms .. ~25s
		}, []string{"feed"}),
		FeedEntriesGauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: sub, Name: "feed_entries",
			Help: "Per-feed entry count from the most recent successful refresh. feed=local is this instance's own observations.",
		}, []string{"feed"}),
		KeywordMatchesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Subsystem: sub, Name: "keyword_matches_total",
			Help: "Torrents dropped before storage because the name or a file path matched the built-in banned-keyword list. source=crawler|import.",
		}, []string{"source"}),
		StoredMatchesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Subsystem: sub, Name: "stored_matches_total",
			Help: "Stored torrents whose name or a file path matched the built-in banned-keyword list. source=processor|sweep. outcome=removed (deleted with no copy kept) or hidden_protected (tied to the operator's own tools: kept and hidden).",
		}, []string{"source", "outcome"}),
		SweepRowsScanned: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: sub, Name: "sweep_rows_scanned",
			Help: "Rows the sweep of stored rows has read in this pass, by table. In a side table only the rows whose torrent is gone are tested.",
		}, []string{"table"}),
		SweepMatches: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: sub, Name: "sweep_matches",
			Help: "Rows the sweep found matching the built-in banned-keyword list in this pass, by table. outcome=removed|hidden_protected with CSAM_SWEEP_MODE=apply, would_remove|would_hide_protected with dry_run.",
		}, []string{"table", "outcome"}),
		SweepDone: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: sub, Name: "sweep_done",
			Help: "1 once the sweep of stored rows has finished its pass for the current list and CSAM_SWEEP_MODE, 0 before that and with the sweep off.",
		}),
		SweepBatchErrorsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: ns, Subsystem: sub, Name: "sweep_batch_errors_total",
			Help: "Sweep batches that failed since this process started. The sweep waits and tries the same row again, so a growing count with sweep_done at 0 means it is stuck.",
		}),
		EntriesGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: ns, Subsystem: sub, Name: "entries",
			Help: "Total double-hash entries in the active bloom filter.",
		}),
		ExportTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: ns, Subsystem: sub, Name: "export_total",
			Help: "Self-export outcomes. outcome=local_ok|local_err|upstream_ok|upstream_err|skipped_no_match.",
		}, []string{"outcome"}),
	}
}

// Collectors returns the slice for fx-group registration.
func (m *Metrics) Collectors() []prometheus.Collector {
	return []prometheus.Collector{
		m.PrefetchBlocksTotal,
		m.LookupsTotal,
		m.FeedRefreshTotal,
		m.FeedRefreshDurationSeconds,
		m.FeedEntriesGauge,
		m.KeywordMatchesTotal,
		m.StoredMatchesTotal,
		m.SweepRowsScanned,
		m.SweepMatches,
		m.SweepDone,
		m.SweepBatchErrorsTotal,
		m.EntriesGauge,
		m.ExportTotal,
	}
}

// setSweep publishes the saved sweep state. Every table and both outcomes of
// the mode are set, so a pass that found nothing reads as 0, not as absent.
func (m *Metrics) setSweep(st sweepState) {
	outcomes := []Outcome{OutcomeRemoved, OutcomeHiddenProtected}
	if st.Mode == SweepDryRun {
		outcomes = []Outcome{OutcomeWouldRemove, OutcomeWouldHide}
	}
	for _, src := range sweepSources {
		m.SweepRowsScanned.WithLabelValues(src.table).Set(float64(st.Scanned[src.table]))
		for _, o := range outcomes {
			m.SweepMatches.WithLabelValues(src.table, string(o)).Set(float64(st.Matches[src.table][o]))
		}
	}
	done := 0.0
	if st.Done {
		done = 1
	}
	m.SweepDone.Set(done)
}
