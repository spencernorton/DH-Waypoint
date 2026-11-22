package dhtms

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ReadMode controls how DH-Waypoint interacts with a co-located dh-tmsd sidecar.
type ReadMode string

const (
	// ReadModeOff disables all DH-TMS traffic (default).
	ReadModeOff ReadMode = "off"
	// ReadModeShadow serves responses from PostgreSQL while shadowing reads to DH-TMS
	// to verify result parity, visibility invariants, and latency.
	ReadModeShadow ReadMode = "shadow"
	// ReadModeServe serves eligible read paths from DH-TMS with fail-closed lease checks.
	ReadModeServe ReadMode = "serve"
)

const (
	defaultTimeout = 500 * time.Millisecond
)

// Config holds runtime settings for the DH-TMS sidecar client.
type Config struct {
	Mode             ReadMode
	Endpoint         string
	AuthToken        string
	Timeout          time.Duration
	ShadowSampleRate float64
}

// DefaultConfig returns a disabled client configuration.
func DefaultConfig() Config {
	return Config{
		Mode:             ReadModeOff,
		Timeout:          defaultTimeout,
		ShadowSampleRate: 1.0,
	}
}

// ConfigFromEnv loads Config from DH_TMS_* environment variables.
func ConfigFromEnv() (Config, error) {
	cfg := DefaultConfig()
	if raw := strings.TrimSpace(os.Getenv("DH_TMS_READ_MODE")); raw != "" {
		cfg.Mode = ReadMode(strings.ToLower(raw))
	}
	if ep := strings.TrimSpace(os.Getenv("DH_TMS_ENDPOINT")); ep != "" {
		cfg.Endpoint = ep
	}
	if tok := strings.TrimSpace(os.Getenv("DH_TMS_AUTH_TOKEN")); tok != "" {
		cfg.AuthToken = tok
	}
	if raw := strings.TrimSpace(os.Getenv("DH_TMS_TIMEOUT_MS")); raw != "" {
		ms, err := strconv.Atoi(raw)
		if err != nil || ms <= 0 {
			return Config{}, fmt.Errorf("invalid DH_TMS_TIMEOUT_MS %q: must be positive integer milliseconds", raw)
		}
		cfg.Timeout = time.Duration(ms) * time.Millisecond
	}
	if raw := strings.TrimSpace(os.Getenv("DH_TMS_SHADOW_SAMPLE_RATE")); raw != "" {
		rate, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return Config{}, fmt.Errorf("invalid DH_TMS_SHADOW_SAMPLE_RATE %q: %w", raw, err)
		}
		cfg.ShadowSampleRate = rate
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks that Config is self-consistent.
func (c Config) Validate() error {
	switch c.Mode {
	case ReadModeOff, ReadModeShadow, ReadModeServe:
	default:
		return fmt.Errorf("invalid DH-TMS read mode %q: expected off, shadow, or serve", c.Mode)
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("invalid DH-TMS timeout %s: must be > 0", c.Timeout)
	}
	if c.ShadowSampleRate < 0 || c.ShadowSampleRate > 1 {
		return fmt.Errorf("invalid DH-TMS shadow sample rate %v: must be in [0, 1]", c.ShadowSampleRate)
	}
	if c.Mode != ReadModeOff {
		ep := strings.TrimSpace(c.Endpoint)
		if ep == "" {
			return fmt.Errorf("DH_TMS_ENDPOINT is required when read mode is %q", c.Mode)
		}
		if !strings.HasPrefix(ep, "unix://") &&
			!strings.HasPrefix(ep, "http://") &&
			!strings.HasPrefix(ep, "https://") {
			return fmt.Errorf("unsupported DH_TMS_ENDPOINT %q: expected unix://, http://, or https://", ep)
		}
		if strings.HasPrefix(ep, "unix://") && strings.TrimPrefix(ep, "unix://") == "" {
			return fmt.Errorf("DH_TMS_ENDPOINT unix:// requires a socket path")
		}
	}
	return nil
}

// Enabled reports whether the client is configured for shadow or serve traffic.
func (c Config) Enabled() bool {
	return c.Mode == ReadModeShadow || c.Mode == ReadModeServe
}

// ShouldShadow reports whether shadow comparisons should run.
func (c Config) ShouldShadow() bool {
	return c.Mode == ReadModeShadow
}

// ShouldServe reports whether eligible read paths may serve from DH-TMS.
func (c Config) ShouldServe() bool {
	return c.Mode == ReadModeServe
}
