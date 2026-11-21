package leancmd

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
)

func TestLeanValidateCommandValidatesAllShippedPresets(t *testing.T) {
	res, err := New(Params{})
	require.NoError(t, err)

	for _, preset := range []string{"1080p-lean.yml", "4k-remux-best.yml", "small-footprint-x265.yml"} {
		var buf bytes.Buffer
		app := &cli.App{
			Name:     "dh-waypoint",
			Commands: []*cli.Command{res.Command},
			Writer:   &buf,
		}
		path := filepath.Join("..", "..", "..", "..", "plugins", "lean-mode", preset)
		require.NoError(t, app.Run([]string{"dh-waypoint", "lean", "validate", path}))

		var out map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
		require.Equal(t, true, out["ok"])
		require.Equal(t, "observe", out["mode"])
		require.Equal(t, false, out["enabled"])
	}
}

func TestLeanCommandRegistersDiffSubcommand(t *testing.T) {
	res, err := New(Params{})
	require.NoError(t, err)
	found := false
	for _, sc := range res.Command.Subcommands {
		if sc.Name == "diff" {
			found = true
			break
		}
	}
	require.True(t, found, "expected lean diff subcommand to be registered")
}
