package search_test

import (
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/stretchr/testify/require"
)

func TestEpisodeKeysCriteriaAndUnlinkedFallbackTimeoutEnv(t *testing.T) {
	t.Setenv("SEARCH_UNLINKED_FALLBACK_TIMEOUT", "65ms")
	require.Equal(t, 65*time.Millisecond, search.ResolveUnlinkedFallbackTimeout())
	t.Setenv("SEARCH_UNLINKED_FALLBACK_TIMEOUT", "")
	require.Equal(t, search.DefaultUnlinkedFallbackTimeout, search.ResolveUnlinkedFallbackTimeout())

	refs := []model.ContentRef{{Type: model.ContentTypeTvShow, Source: "tmdb", ID: "1396"}}
	seasonOnly := make(model.Episodes).AddSeason(2)
	andSeason := search.TorrentContentEpisodeKeysCriteria(refs, seasonOnly).(query.AndCriteria)
	require.Len(t, andSeason.Criteria, 1)
	dbSeason := andSeason.Criteria[0].(query.DBCriteria)
	require.Contains(t, dbSeason.SQL, "torrent_contents_episode_keys")
	require.Contains(t, dbSeason.SQL, "&&")

	singleEp := make(model.Episodes).AddEpisode(2, 5)
	andEp := search.TorrentContentEpisodeKeysCriteria(refs, singleEp).(query.AndCriteria)
	require.Len(t, andEp.Criteria, 1)
	orEp := andEp.Criteria[0].(query.OrCriteria)
	require.Len(t, orEp.Criteria, 1)
	require.Contains(t, orEp.Criteria[0].(query.DBCriteria).SQL, "@>")
}
