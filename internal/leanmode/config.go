// Package leanmode implements the opt-in LEAN MODE catalog curation extension.
//
// LEAN MODE evaluates linked movies and TV episodes against a declarative
// three-tier YAML profile (required -> preferred -> if_found) and keeps only
// the single best release per slot (plus optional warm runner-ups for
// swarm-death and proof-expiry failover). It supports three progressive modes:
//   - observe: compute and persist lean_winners without changing serving or deleting rows.
//   - serve:   narrow consumer APIs to canonical winners while leaving stored rows intact.
//   - prune:   opt-in bounded physical pruning of dominated losers with config-digest
//     tombstones that prevent DHT/importer re-crawl churn.
//
// Disabled by default upstream (Enabled=false): zero background queries, zero
// serving overhead, and zero schema mutations when inactive.
package leanmode

import "time"

// Config controls the LEAN MODE runtime worker and serving integration.
// Registered as "lean_mode" on configfx; env prefix LEAN_MODE_*.
type Config struct {
	// Enabled turns LEAN MODE on. Defaults to false (100% opt-in).
	Enabled bool `yaml:"enabled"`

	// ConfigFile points to the declarative YAML profile defining required,
	// preferred, and if_found rules.
	ConfigFile string `yaml:"config_file"`

	// Interval between incremental reconciliation sweeps.
	Interval time.Duration `yaml:"interval"`

	// TitleBatchSize bounds how many distinct movies/TV shows are reconciled
	// per database batch.
	TitleBatchSize int `yaml:"title_batch_size"`

	// ServeGlobally narrows all default consumer search surfaces (Torznab,
	// REST, GraphQL) to the active profile when the profile mode is "serve"
	// or "prune". When false, only callers explicitly selecting the profile
	// receive lean-filtered responses.
	ServeGlobally bool `yaml:"serve_globally"`
}

// NewDefaultConfig returns the inert default configuration (disabled).
func NewDefaultConfig() Config {
	return Config{
		Enabled:        false,
		ConfigFile:     "",
		Interval:       15 * time.Minute,
		TitleBatchSize: 200,
		ServeGlobally:  false,
	}
}
