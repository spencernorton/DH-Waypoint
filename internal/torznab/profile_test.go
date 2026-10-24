package torznab

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTVDBCapabilityIsTVOnly(t *testing.T) {
	caps := ProfileDefault.Caps()
	require.Contains(t, strings.Split(caps.Searching.TvSearch.SupportedParams, ","), "tvdbid")
	require.NotContains(t, strings.Split(caps.Searching.MovieSearch.SupportedParams, ","), "tvdbid")
}
