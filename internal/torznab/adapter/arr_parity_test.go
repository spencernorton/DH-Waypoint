package adapter

import (
	"encoding/xml"
	"strings"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/torznab"
	"github.com/stretchr/testify/require"
)

func TestEffectiveSearchQueryMusicAudioBookParams(t *testing.T) {
	t.Parallel()

	require.Equal(t, "Pink Floyd Dark Side 1973", effectiveSearchQuery(torznab.SearchRequest{
		Type:   torznab.FunctionMusic,
		Artist: "Pink Floyd",
		Album:  "Dark Side",
		Year:   model.NewNullInt(1973),
	}))
	require.Equal(t, "Pink Floyd Dark Side", effectiveSearchQuery(torznab.SearchRequest{
		Type:   torznab.FunctionAudio,
		Query:  "Pink Floyd",
		Artist: "pink floyd",
		Album:  "Dark Side",
	}))
	require.Equal(t, "Frank Herbert Dune 1965", effectiveSearchQuery(torznab.SearchRequest{
		Type:   torznab.FunctionBook,
		Author: "Frank Herbert",
		Title:  "Dune",
		Year:   model.NewNullInt(1965),
	}))
}

func TestSearchOptionsAcceptAudioAndYear(t *testing.T) {
	t.Parallel()

	for _, req := range []torznab.SearchRequest{
		{Type: torznab.FunctionAudio, Artist: "Bach", Album: "Suites"},
		{Type: torznab.FunctionMusic, Artist: "Miles Davis", Year: model.NewNullInt(1959)},
		{Type: torznab.FunctionBook, Author: "Ursula Le Guin", Title: "Earthsea"},
		{Type: torznab.FunctionMovie, Query: "Dune", Year: model.NewNullInt(2021)},
		{Type: torznab.FunctionTV, Query: "Andor", Year: model.NewNullInt(2022)},
	} {
		opts, err := searchRequestToQueryOptions(req)
		require.NoError(t, err, "type=%s", req.Type)
		require.NotEmpty(t, opts, "type=%s", req.Type)
	}
}

func TestTorznabResultEmitsFreeleechAndZeroTotalResponse(t *testing.T) {
	t.Parallel()

	emptyRes := torrentContentResultToTorznabResult(torznab.SearchRequest{
		Profile: torznab.ProfileDefault,
	}, search.TorrentContentResult{}, false)
	rawXML, err := emptyRes.XML()
	require.NoError(t, err)
	xmlStr := string(rawXML)
	require.Contains(t, xmlStr, `offset="0"`)
	require.Contains(t, xmlStr, `total="0"`)

	item := torrentContentResultItemToTorznabResultItem(search.TorrentContentResultItem{
		TorrentContent: model.TorrentContent{
			Torrent: model.Torrent{
				Name: "Example.Movie.2024.1080p.mkv",
				Size: 1024,
			},
		},
	}, false)
	attrs := make(map[string]string, len(item.TorznabAttrs))
	for _, a := range item.TorznabAttrs {
		attrs[a.AttrName] = a.AttrValue
	}
	require.Equal(t, "0", attrs[torznab.AttrDownloadVolumeFactor])
	require.Equal(t, "1", attrs[torznab.AttrUploadVolumeFactor])

	// Verify caps advertise audio, artist/album, author/title, and year.
	caps := torznab.ProfileDefault.Caps()
	capsBytes, err := xml.Marshal(caps)
	require.NoError(t, err)
	require.Equal(t, "yes", caps.Searching.AudioSearch.Available)
	require.True(t, strings.Contains(caps.Searching.MovieSearch.SupportedParams, "year"))
	require.True(t, strings.Contains(caps.Searching.TvSearch.SupportedParams, "year"))
	require.True(t, strings.Contains(caps.Searching.MusicSearch.SupportedParams, "artist,album,year"))
	require.True(t, strings.Contains(caps.Searching.BookSearch.SupportedParams, "author,title,year"))
	require.Contains(t, string(capsBytes), `audio-search available="yes"`)
}
