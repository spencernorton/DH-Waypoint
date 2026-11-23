// Package eventprune bounds history tables that otherwise grow for the life of
// the database: processor feedback events, repeated qBittorrent state
// observations, superseded verdict events, and historical processor application
// result payloads.
package eventprune

import (
	"errors"
	"time"
)

// Config is the "eventprune" section (env EVENTPRUNE_*). A zero retention
// turns that table's pruner off.
type Config struct {
	Interval time.Duration
	// BatchSize is the most rows one statement examines. A cycle runs at most
	// maxBatches statements per table.
	BatchSize int
	// FeedbackRetention is how long a processor_feedback_events row is kept,
	// delivered or not.
	FeedbackRetention time.Duration
	// QBStateRetention is how long a qBittorrent state observation in
	// label_evidence is kept once a newer one exists for the same torrent.
	QBStateRetention time.Duration
	// VerdictEventRetention is how long a torrent_verdict_events row is kept
	// once a newer event exists for the same torrent.
	VerdictEventRetention time.Duration
	// ApplicationResultsRetention is how long full JSON receipts in
	// processor_applications.results are kept before compacting results to '[]'
	// while preserving revision columns for queue idempotency.
	ApplicationResultsRetention time.Duration
	// DHTMSOutboxRetention bounds dhtms_outbox rows on PostgreSQL-only installs
	// where no dh-tmsd sync worker drains the outbox.
	DHTMSOutboxRetention time.Duration
}

func NewDefaultConfig() Config {
	return Config{
		Interval:                    time.Hour,
		BatchSize:                   5000,
		FeedbackRetention:           30 * 24 * time.Hour,
		QBStateRetention:            90 * 24 * time.Hour,
		VerdictEventRetention:       180 * 24 * time.Hour,
		ApplicationResultsRetention: 90 * 24 * time.Hour,
		DHTMSOutboxRetention:        24 * time.Hour,
	}
}

const (
	// Durations have no day unit; the floors stop a value such as 30m, meant
	// as 30 days, from emptying a table.
	minFeedbackRetention = 24 * time.Hour
	// The availability policy reads up to a week of qBittorrent history.
	minHistoryRetention = 30 * 24 * time.Hour
	minOutboxRetention  = time.Hour
)

func (c Config) enabled() bool {
	return c.FeedbackRetention > 0 || c.QBStateRetention > 0 || c.VerdictEventRetention > 0 || c.ApplicationResultsRetention > 0 || c.DHTMSOutboxRetention > 0
}

func (c Config) Validate() error {
	if c.Interval < time.Minute || c.BatchSize < 1 || c.BatchSize > 100000 {
		return errors.New("eventprune: interval must be at least 1m and batch_size between 1 and 100000")
	}
	if c.FeedbackRetention != 0 && c.FeedbackRetention < minFeedbackRetention {
		return errors.New("eventprune: feedback_retention must be 0 (off) or at least 24h")
	}
	if (c.QBStateRetention != 0 && c.QBStateRetention < minHistoryRetention) ||
		(c.VerdictEventRetention != 0 && c.VerdictEventRetention < minHistoryRetention) ||
		(c.ApplicationResultsRetention != 0 && c.ApplicationResultsRetention < minHistoryRetention) {
		return errors.New("eventprune: qb_state_retention, verdict_event_retention and application_results_retention must be 0 (off) or at least 720h")
	}
	if c.DHTMSOutboxRetention != 0 && c.DHTMSOutboxRetention < minOutboxRetention {
		return errors.New("eventprune: dhtms_outbox_retention must be 0 (off) or at least 1h")
	}
	return nil
}
