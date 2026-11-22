package httpserver

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeMetricTypeClampsUnknownValues(t *testing.T) {
	t.Parallel()
	for _, valid := range []string{"search", "tvsearch", "movie", "music", "audio", "book", "caps"} {
		require.Equal(t, valid, normalizeMetricType(valid))
	}
	require.Equal(t, "empty", normalizeMetricType(""))
	require.Equal(t, "other", normalizeMetricType("random-attacker-cardinality-12345"))
	require.Equal(t, "other", normalizeMetricType("MOVIE"))
}
