package httpserver_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// The shipped Compose files must hand the API key to the container and keep
// the HTTP port off the network until the operator opens it.
func TestComposeFilesForwardTheAPIKeyAndBindLocally(t *testing.T) {
	for _, path := range []string{"../../docker-compose.yml", "../../examples/docker-compose.public.yml"} {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		var compose struct {
			Services map[string]struct {
				Environment map[string]any
				Ports       []string
			}
		}
		require.NoError(t, yaml.Unmarshal(raw, &compose), path)
		app := compose.Services["dh-waypoint"]
		require.Contains(t, app.Environment, "HTTP_SERVER_API_KEY", path)
		require.Contains(t, app.Environment, "HTTP_SERVER_CORS_ALLOWED_ORIGINS", path)
		// A sweep mode set in .env must reach the container; unset keeps the built-in default.
		require.Equal(t, "${CSAM_SWEEP_MODE:-apply}", app.Environment["CSAM_SWEEP_MODE"], path)
		var httpPorts []string
		for _, port := range app.Ports {
			if strings.HasSuffix(port, ":3333") {
				httpPorts = append(httpPorts, port)
			}
		}
		require.Equal(t, []string{"${DH_WAYPOINT_HTTP_BIND:-127.0.0.1}:${DH_WAYPOINT_HTTP_PORT:-3333}:3333"}, httpPorts, path)
	}
}
