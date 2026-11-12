package classifier

import (
	"context"
	"strconv"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/classifier/classification"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/tmdb"
	tmdb_mocks "github.com/spencernorton/DH-Waypoint/internal/tmdb/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

type stubContentBackend struct {
	search.Search
	items []search.ContentResultItem
}

func (s stubContentBackend) Content(_ context.Context, _ ...query.Option) (search.ContentResult, error) {
	return search.ContentResult{Items: s.items}, nil
}

func tvDetailsFixture(id int64, name, firstAir string, numSeasons int, seasonEps map[int]int, pop float32, votes int64) tmdb.TvDetailsResponse {
	d := tmdb.TvDetailsResponse{
		ID:              id,
		Name:            name,
		OriginalName:    name,
		FirstAirDate:    firstAir,
		NumberOfSeasons: numSeasons,
		Popularity:      pop,
		VoteCount:       votes,
	}
	for sNum, epCnt := range seasonEps {
		d.Seasons = append(d.Seasons, struct {
			AirDate      string `json:"air_date"`
			EpisodeCount int    `json:"episode_count"`
			ID           int64  `json:"id"`
			Name         string `json:"name"`
			Overview     string `json:"overview"`
			PosterPath   string `json:"poster_path"`
			SeasonNumber int    `json:"season_number"`
		}{
			SeasonNumber: sNum,
			EpisodeCount: epCnt,
		})
	}
	return d
}

func TestShortTitlePrecisionGate_BlocksDifferentShowEvenWhenFuzzyDisabled(t *testing.T) {
	t.Parallel()

	negatives := []struct {
		query string
		cand  string
	}{
		{"Drifter", "Home Drifter"}, // distance 5 on 7-char title, different first token
		{"Solitude", "Home Solitude"},
		{"Vex", "Vexation"},  // distance 5 on 3-char title
		{"Pulse", "Pulsing"}, // distance 3 > scaled threshold(1)
	}

	for _, tc := range negatives {
		for _, fuzzy := range []bool{false, true} {
			_, ok := runFuzzyMatch(t, tc.query, 0, []matchItem{{titles: []string{tc.cand}, year: 2017}}, fuzzy)
			assert.False(t, ok, "fuzzy=%v: %q must NOT attach to %q", fuzzy, tc.query, tc.cand)
		}
	}

	// Exact normalized title beats a 1-edit plural candidate within compatible year.
	items := []matchItem{
		{titles: []string{"LongVoyagers"}, year: 2020},
		{titles: []string{"LongVoyager"}, year: 2019},
	}
	got, ok := runFuzzyMatch(t, "LongVoyager", 2020, items, true)
	require.True(t, ok)
	assert.Equal(t, []string{"LongVoyager"}, got.titles)
}

func TestTVShowDisambiguation_LocalFallthroughAndTMDBSeasonFeasibility(t *testing.T) {
	t.Parallel()

	// Local DB initially contains two wrong candidates that used to trap "Drifter.S11E01":
	// 1) "Home Drifter" (2017, different first token, edit distance 5)
	// 2) "Drifter" (2023, 1-season homonym cached earlier)
	homeDrifter := model.Content{
		Type:        model.ContentTypeTvShow,
		Source:      model.SourceTmdb,
		ID:          "74001",
		Title:       "Home Drifter",
		ReleaseYear: 2017,
		Popularity:  model.NewNullFloat32(25.0),
	}
	shortHomonym, err := tmdb.TvShowDetailsToTvShowModel(
		tvDetailsFixture(231001, "Drifter", "2023-03-01", 1, map[int]int{1: 6}, 120.0, 40),
	)
	require.NoError(t, err)

	local := NewLocalSearch(stubContentBackend{
		items: []search.ContentResultItem{
			{Content: homeDrifter},
			{Content: shortHomonym},
		},
	}, Config{FuzzyMatchEnabled: false})

	tmdbClient := tmdb_mocks.NewClient(t)
	anyCtx := mock.MatchedBy(func(ctx any) bool {
		_, ok := ctx.(context.Context)
		return ok
	})

	// Even if TMDB returns the 1-season 2023 homonym FIRST with higher transient
	// popularity, requesting S11E01 must probe TvDetails, reject the 1-season
	// homonym, and attach the 11-season 2015 flagship series (ID 63001).
	tmdbClient.On("SearchTv", anyCtx, tmdb.SearchTvRequest{
		Query:        "Drifter",
		IncludeAdult: true,
	}).Return(tmdb.SearchTvResponse{
		Results: []tmdb.SearchTvResult{
			{ID: 231001, Name: "Drifter", OriginalName: "Drifter", FirstAirDate: "2023-03-01", Popularity: 120.0, VoteCount: 40},
			{ID: 63001, Name: "Drifter", OriginalName: "Drifter", FirstAirDate: "2015-06-18", Popularity: 85.0, VoteCount: 510},
		},
	}, nil).Once()

	tmdbClient.On("TvDetails", anyCtx, tmdb.TvDetailsRequest{
		SeriesID:         231001,
		AppendToResponse: []string{"external_ids", "alternative_titles", "translations"},
	}).Return(tvDetailsFixture(231001, "Drifter", "2023-03-01", 1, map[int]int{1: 6}, 120.0, 40), nil).Once()

	tmdbClient.On("TvDetails", anyCtx, tmdb.TvDetailsRequest{
		SeriesID:         63001,
		AppendToResponse: []string{"external_ids", "alternative_titles", "translations"},
	}).Return(tvDetailsFixture(63001, "Drifter", "2015-06-18", 11, map[int]int{11: 12}, 85.0, 510), nil).Once()

	comp := compiler{
		options: []compilerOption{
			compilerFeatures(defaultFeatures),
			celEnvOption,
		},
		dependencies: dependencies{
			search:                local,
			tmdbClient:            tmdbClient,
			singleEpisodeMaxBytes: 4 * testGiB,
		},
	}
	source, err := yamlSourceProvider{rawSourceProvider: coreSourceProvider{}}.source()
	require.NoError(t, err)
	runner, err := comp.Compile(source)
	require.NoError(t, err)

	res, err := runner.Run(context.Background(), "default", Flags{}, model.Torrent{
		Name:        "Drifter.S11E01.1080p.WEB.h264.mkv",
		FilesStatus: model.FilesStatusSingle,
		Extension:   model.NewNullString("mkv"),
		Size:        uint(900 * 1024 * 1024),
	})
	require.NoError(t, err)
	require.NotNil(t, res.Content)
	assert.Equal(t, strconv.Itoa(63001), res.Content.ID)
	assert.Equal(t, model.Year(2015), res.Content.ReleaseYear)
	assert.Equal(t, model.Episodes{11: {1: {}}}, res.Episodes)
}

func TestSeasonPackBackstop_PreservesConfirmedCompactSeasonPacks(t *testing.T) {
	t.Parallel()

	const ceiling = int64(4 * testGiB)

	// 1. Multi-video-file compact HEVC season pack (10 x 280 MiB = 2.8 GiB <= 4 GiB) keeps S02.
	var packFiles []model.TorrentFile
	for i := 1; i <= 10; i++ {
		packFiles = append(packFiles, model.TorrentFile{
			Index:     uint(i - 1),
			Path:      "Signal.Harbor.S02E" + strconv.Itoa(i) + ".720p.x265.mkv",
			Extension: model.NewNullString("mkv"),
			Size:      uint(280 * 1024 * 1024),
		})
	}
	eps := runBackstopTest(t, ceiling, model.Torrent{
		Name:        "Signal.Harbor.S02.720p.WEBRip.x265-SYNTH",
		Size:        uint(2800 * 1024 * 1024),
		FilesStatus: model.FilesStatusMulti,
		Files:       packFiles,
		Hint:        model.TorrentHint{ContentType: model.ContentTypeTvShow},
	})
	assert.Equal(t, model.Episodes{2: {}}, eps, "multi-video-file season pack <= 4 GiB must keep season claim")

	// 2. Single episode + small sample.mkv (FilesStatusMulti, only 1 primary video file) is STILL demoted.
	epsWithSample := runBackstopTest(t, ceiling, model.Torrent{
		Name:        "Signal.Harbor.S02.HDTV.x264-SYNTH",
		Size:        uint(812 * 1024 * 1024),
		FilesStatus: model.FilesStatusMulti,
		Files: []model.TorrentFile{
			{Index: 0, Path: "signal.harbor.s02.hdtv.mkv", Extension: model.NewNullString("mkv"), Size: uint(800 * 1024 * 1024)},
			{Index: 1, Path: "Sample/sample-signal.harbor.mkv", Extension: model.NewNullString("mkv"), Size: uint(12 * 1024 * 1024)},
		},
		Hint: model.TorrentHint{ContentType: model.ContentTypeTvShow},
	})
	assert.Empty(t, epsWithSample, "single episode + sample.mkv <= 4 GiB must still be demoted")

	// 3. Multi-season pack (S01-S02) <= 4 GiB keeps {1:{}, 2:{}}.
	epsMultiSeason := runBackstopTest(t, ceiling, model.Torrent{
		Name:        "Signal.Harbor.S01-S02.720p.x265-SYNTH",
		Size:        uint(2700 * 1024 * 1024),
		FilesStatus: model.FilesStatusNoInfo,
		Hint:        model.TorrentHint{ContentType: model.ContentTypeTvShow},
	})
	assert.Equal(t, model.Episodes{1: {}, 2: {}}, epsMultiSeason, "multi-season pack <= 4 GiB must keep season claims")

	// 4. Non-single-file torrent explicitly titled "Season 2" / "COMPLETE" keeps {2:{}},
	//    and EnrichTypeLocally preserves the exact same behavior.
	explicitPack := model.Torrent{
		Name:        "Signal Harbor - Season 2 COMPLETE WEB-DL 720p",
		Size:        uint(3000 * 1024 * 1024),
		FilesStatus: model.FilesStatusMulti,
		Hint:        model.TorrentHint{ContentType: model.ContentTypeTvShow},
	}
	assert.Equal(t, model.Episodes{2: {}}, runBackstopTest(t, ceiling, explicitPack))

	enriched, err := EnrichTypeLocally(
		context.Background(),
		explicitPack,
		classification.Result{
			ContentAttributes: classification.ContentAttributes{
				ContentType: model.NewNullContentType(model.ContentTypeTvShow),
			},
		},
		nil,
		LocalTypeEnrichmentOptions{SingleEpisodeMaxBytes: ceiling},
	)
	require.NoError(t, err)
	assert.Equal(t, model.Episodes{2: {}}, enriched.Episodes)
}
