package eventprune

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConfigDefaultsAndFloors(t *testing.T) {
	cfg := NewDefaultConfig()
	require.NoError(t, cfg.Validate())
	require.True(t, cfg.enabled())
	require.GreaterOrEqual(t, cfg.FeedbackRetention, 30*24*time.Hour)
	require.GreaterOrEqual(t, cfg.QBStateRetention, 90*24*time.Hour)
	require.GreaterOrEqual(t, cfg.VerdictEventRetention, 90*24*time.Hour)

	require.False(t, Config{Interval: time.Hour, BatchSize: 1}.enabled())

	// EVENTPRUNE_FEEDBACK_RETENTION=30m is 30 minutes, not 30 days.
	for _, set := range []func(*Config){
		func(c *Config) { c.FeedbackRetention = 30 * time.Minute },
		func(c *Config) { c.QBStateRetention = 24 * time.Hour },
		func(c *Config) { c.VerdictEventRetention = 24 * time.Hour },
		func(c *Config) { c.BatchSize = 0 },
		func(c *Config) { c.Interval = time.Second },
	} {
		bad := NewDefaultConfig()
		set(&bad)
		require.Error(t, bad.Validate())
	}
}
