package setupcmd

import (
	"bytes"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/database/postgres"
	"github.com/spencernorton/DH-Waypoint/internal/processor"
	"github.com/spencernorton/DH-Waypoint/internal/processors"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
)

func TestDoctorDefaultDoesNotRequireSQLDB(t *testing.T) {
	res, err := New(Params{
		Processing: processor.Config{Mode: processor.ModeBasic},
		Processors: processors.Config{PluginsDir: ""},
		Postgres:   postgres.Config{DSN: "postgres://example/test"},
	})
	require.NoError(t, err)

	var out bytes.Buffer
	app := &cli.App{
		Writer:   &out,
		Commands: []*cli.Command{res.DoctorCommand},
	}
	require.NoError(t, app.Run([]string{"dh-waypoint", "doctor"}))
	require.Contains(t, out.String(), "[ok  ] processing.mode: basic")
	require.Contains(t, out.String(), "[ok  ] postgres: DSN configured")
}
