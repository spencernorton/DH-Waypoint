package searchcmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"
)

func TestSearchBM25CommandTree(t *testing.T) {
	res, err := New(Params{})
	require.NoError(t, err)
	require.Equal(t, "search", res.Command.Name)
	require.Len(t, res.Command.Subcommands, 1)

	bm25 := res.Command.Subcommands[0]
	require.Equal(t, "bm25", bm25.Name)

	names := make([]string, 0, len(bm25.Subcommands))
	for _, sub := range bm25.Subcommands {
		names = append(names, sub.Name)
	}
	require.Equal(t, []string{"status", "enable", "verify", "disable"}, names)
}

func TestSearchBM25StatusWithoutDatabaseErrorsCleanly(t *testing.T) {
	res, err := New(Params{})
	require.NoError(t, err)

	var out bytes.Buffer
	app := &cli.App{
		Writer:         &out,
		Commands:       []*cli.Command{res.Command},
		ExitErrHandler: func(*cli.Context, error) {},
	}
	err = app.Run([]string{"dh-waypoint", "search", "bm25", "status"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "database is not configured")
}
