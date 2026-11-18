package config

import (
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/spencernorton/DH-Waypoint/internal/config/configresolver"
	"github.com/stretchr/testify/require"
)

type sampleSecretSpec struct {
	PublicHost string
	APIKey     string
	APIKeys    string
	Password   string
	Database   string
}

func TestConfigShowMasksSecretPathsAndURLPasswordsPreservingRuntimeValues(t *testing.T) {
	synKey := strings.Repeat("k", 18)
	synPass := strings.Repeat("p", 12)
	synDBPass := strings.Repeat("d", 12)
	dsn := "postgres://app:" + synDBPass + "@" + "127.0.0.1:5432/waypoint"

	node, err := resolveRootNode(
		[]configresolver.Resolver{configresolver.NewEnv(map[string]string{
			"SAMPLE_API_KEY":  synKey,
			"SAMPLE_PASSWORD": synPass,
		})},
		validator.New(),
		Spec{Key: "sample", DefaultValue: sampleSecretSpec{
			PublicHost: "0.0.0.0",
			Database:   dsn,
		}},
	)
	require.NoError(t, err)
	typed := node.Value.(sampleSecretSpec)
	require.Equal(t, synKey, typed.APIKey, "runtime config keeps real value")
	require.Equal(t, synPass, typed.Password)

	require.Equal(t, "[redacted]", node.ChildMap["api_key"].ValueLabel)
	require.Equal(t, "''", node.ChildMap["api_keys"].ValueLabel, "unset field remains distinguishable from set field")
	require.Equal(t, "[redacted]", node.ChildMap["password"].ValueLabel)
	require.NotContains(t, node.ChildMap["database"].DefaultLabel, synDBPass)
	require.Contains(t, node.ChildMap["database"].DefaultLabel, "postgres://app:***@")
}
