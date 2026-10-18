package config_test

import (
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/spencernorton/DH-Waypoint/internal/config"
	"github.com/spencernorton/DH-Waypoint/internal/config/configresolver"
	"github.com/stretchr/testify/require"
)

type strictSample struct {
	Mode     string
	Keywords map[string][]string
	Nested   struct {
		Enabled bool
	}
}

func buildStrict(t *testing.T, m map[string]interface{}) error {
	t.Helper()
	_, err := config.New(config.Params{
		Specs: []config.Spec{{Key: "sample", DefaultValue: strictSample{Mode: "full"}}},
		Resolvers: []configresolver.Resolver{
			configresolver.NewMap(m, validator.New(), configresolver.WithKey("test.yml")),
		},
		Validate: validator.New(),
	})
	return err
}

func TestStrictMapKeysAcceptKnownAndMapDescendants(t *testing.T) {
	require.NoError(t, buildStrict(t, map[string]interface{}{
		"sample": map[string]interface{}{
			"mode":     "basic",
			"keywords": map[string]interface{}{"banned": []interface{}{"x"}},
			"nested":   map[string]interface{}{"enabled": true},
		},
	}))
}

func TestStrictMapKeysRejectTypos(t *testing.T) {
	err := buildStrict(t, map[string]interface{}{
		"sample": map[string]interface{}{"mod": "basic"},
	})
	require.ErrorContains(t, err, "unknown configuration key")
	require.ErrorContains(t, err, "sample.mod")

	err = buildStrict(t, map[string]interface{}{
		"nope": map[string]interface{}{"x": 1},
	})
	require.ErrorContains(t, err, "nope.x")
}
