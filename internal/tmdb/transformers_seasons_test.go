package tmdb

import (
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTvShowDetailsToTvShowModel_PersistsSeasonAndEpisodeCounts(t *testing.T) {
	t.Parallel()

	details := TvDetailsResponse{
		ID:              63001,
		Name:            "Synthetic Horizon",
		OriginalName:    "Synthetic Horizon",
		FirstAirDate:    "2015-06-18",
		NumberOfSeasons: 11,
	}
	details.Seasons = []struct {
		AirDate      string `json:"air_date"`
		EpisodeCount int    `json:"episode_count"`
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		Overview     string `json:"overview"`
		PosterPath   string `json:"poster_path"`
		SeasonNumber int    `json:"season_number"`
	}{
		{SeasonNumber: 0, EpisodeCount: 5, Name: "Specials"},
		{SeasonNumber: 1, EpisodeCount: 10, Name: "Season 1"},
		{SeasonNumber: 11, EpisodeCount: 12, Name: "Season 11"},
	}

	tvModel, err := TvShowDetailsToTvShowModel(details)
	require.NoError(t, err)

	attrMap := make(map[string]string)
	for _, a := range tvModel.Attributes {
		if a.Source == model.SourceTmdb {
			attrMap[a.Key] = a.Value
		}
	}

	assert.Equal(t, "11", attrMap[AttrKeyNumberOfSeasons])
	assert.Equal(t, "10", attrMap[AttrKeySeasonEpisodesPrefix+"1"])
	assert.Equal(t, "12", attrMap[AttrKeySeasonEpisodesPrefix+"11"])
	assert.NotContains(t, attrMap, AttrKeySeasonEpisodesPrefix+"0")
}
