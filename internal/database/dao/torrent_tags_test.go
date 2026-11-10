package dao

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTagDeleteRefusesACallWithoutTorrents(t *testing.T) {
	// The zero value has no database, so reaching a query would panic.
	require.Error(t, (&torrentTag{}).Delete(context.Background(), nil, []string{"tag"}))
}
