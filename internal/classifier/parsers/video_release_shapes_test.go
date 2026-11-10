package parsers

import (
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/classifier/classification"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seasons(from, to int) model.Episodes {
	e := model.Episodes{}
	for s := from; s <= to; s++ {
		e = e.AddSeason(s)
	}
	return e
}

func episodes(season, from, to int) model.Episodes {
	e := model.Episodes{}
	for ep := from; ep <= to; ep++ {
		e = e.AddEpisode(season, ep)
	}
	return e
}

// TestReleaseNameShapes covers common release-name shapes: season and episode
// ranges, a separator between season and episode, a spelled-out season, fansub
// numbering, a resolution in brackets or ahead of the episode marker, an exact
// height next to a loose word such as HD, and a word that only looks like a
// season marker. Both parse modes must agree on them.
func TestReleaseNameShapes(t *testing.T) {
	t.Parallel()

	const (
		tv    = model.ContentTypeTvShow
		movie = model.ContentTypeMovie
	)

	cases := []struct {
		name         string
		wantType     model.ContentType
		wantTitle    string
		wantYear     model.Year
		wantEpisodes model.Episodes
		wantRes      model.VideoResolution
	}{
		// Season ranges: the whole range is the marker, not its last season.
		{"Show.Name.S01-S03.COMPLETE.1080p.BluRay.x264-GRP", tv, "Show Name", 0, seasons(1, 3), model.VideoResolutionV1080p},
		{"Peep Show S01-S09 720p WEB-DL H265 BONE", tv, "Peep Show", 0, seasons(1, 9), model.VideoResolutionV720p},
		// Episode range that repeats the season on its end.
		{"Show.Name.S01E01-S01E03.720p.WEB.x264-GRP", tv, "Show Name", 0, episodes(1, 1, 3), model.VideoResolutionV720p},
		// A separator between the season and the episode.
		{"Show.Name.S01.E05.720p.HDTV.x264-GRP", tv, "Show Name", 0, episodes(1, 5, 5), model.VideoResolutionV720p},
		// Fansub "S<season> - <episode>" is one episode, not a season range.
		{"[SubsPlease] Show Name S2 - 05 (1080p) [ABCD1234].mkv", tv, "Show Name", 0, episodes(2, 5, 5), model.VideoResolutionV1080p},
		// Fansub absolute numbering keeps the bracketed resolution.
		{"[SubsPlease] Show Name - 05 (1080p) [ABCD1234].mkv", tv, "Show Name", 0, nil, model.VideoResolutionV1080p},
		// A spelled-out season with a dot separator is a season pack.
		{"Show.Name.Season.1.Complete.720p.BluRay.x264-GRP", tv, "Show Name", 0, seasons(1, 1), model.VideoResolutionV720p},
		// An exact height beats a loose word, wherever each one sits.
		{"Movie Name 2020 HD 1080p BluRay x264-GRP", movie, "Movie Name", 2020, nil, model.VideoResolutionV1080p},
		{"Movie.Name.2019.UHD.BluRay.1080p.x264-GRP", movie, "Movie Name", 2019, nil, model.VideoResolutionV1080p},
		// A resolution ahead of the episode marker is not part of the title.
		{"Show.2160p.S01E01.WEB.h264-GRP", tv, "Show", 0, episodes(1, 1, 1), model.VideoResolutionV2160p},
		// "S1ngle" is a word, not season 1.
		{"The.S1ngle.Life.2020.1080p.WEB-DL.x264-GRP", movie, "The S1ngle Life", 2020, nil, model.VideoResolutionV1080p},

		// The first marker wins; a release group that looks like a season
		// neither replaces it nor makes a film a show.
		{"Show.Name.S03E03.1080p.WEB.x264-S4LVE", tv, "Show Name", 0, episodes(3, 3, 3), model.VideoResolutionV1080p},
		{"Movie.Name.2021.1080p.WEB.x264-S4LVE", movie, "Movie Name", 2021, nil, model.VideoResolutionV1080p},
		// "Season" and a year is a title word and a year.
		{"Happy.Season.2020.1080p.WEBRip.x264-GRP", movie, "Happy Season", 2020, nil, model.VideoResolutionV1080p},
		// A season is not a range into the resolution after it.
		{"Show Name - Season 2 - 2160p WEB-DL x265-GRP", tv, "Show Name", 0, seasons(2, 2), model.VideoResolutionV2160p},
		// More separators, a versioned fansub episode, a cross-season range.
		{"Show Name S01 EP(01-08) 720p WEB-DL x264-GRP", tv, "Show Name", 0, episodes(1, 1, 8), model.VideoResolutionV720p},
		{"Show Name - S02xE07-08", tv, "Show Name", 0, episodes(2, 7, 8), ""},
		{"[SubsPlease] Show Name S2 - 06v2 (720p) [ABCD1234].mkv", tv, "Show Name", 0, episodes(2, 6, 6), model.VideoResolutionV720p},
		{"Show.Name.S01E01-S02E10.1080p.WEB.x264-GRP", tv, "Show Name", 0, seasons(1, 2), model.VideoResolutionV1080p},
		// A lone S before an NxN marker is a title word.
		{"Show Name S 2x06 HDTV XviD-GRP", tv, "Show Name S", 0, episodes(2, 6, 6), ""},

		// Shapes that already parsed correctly and must stay that way.
		{"Show.Name.S05D2.PAL.DVDR-GRP", tv, "Show Name", 0, seasons(5, 5), ""},
		{"Show Name S01 x264-GRP", tv, "Show Name", 0, seasons(1, 1), ""},
		{"Show.Name.S01E05.720p.HDTV.x264-GRP", tv, "Show Name", 0, episodes(1, 5, 5), model.VideoResolutionV720p},
		{"Show.Name.S01E05.HDTV.x264-LOL", tv, "Show Name", 0, episodes(1, 5, 5), ""},
		{"Movie.Name.2005.DVDRip.XviD-GRP", movie, "Movie Name", 2005, nil, ""},
		{"Show.Name.S02.1080p.WEB-DL.x264-GRP", tv, "Show Name", 0, seasons(2, 2), model.VideoResolutionV1080p},
		{"Show Name Season 1-5 Complete 720p", tv, "Show Name", 0, seasons(1, 5), model.VideoResolutionV720p},
		{"Show.Name.S01E01E02.1080p.WEB.x264-GRP", tv, "Show Name", 0, episodes(1, 1, 2), model.VideoResolutionV1080p},
		{"Show.Name.S01E01-E03.1080p.WEB.x264-GRP", tv, "Show Name", 0, episodes(1, 1, 3), model.VideoResolutionV1080p},
		{"Movie.Name.2021.UHD.BluRay.2160p.x265-GRP", movie, "Movie Name", 2021, nil, model.VideoResolutionV2160p},
		{"Movie Name 2018 HD BluRay x264-GRP", movie, "Movie Name", 2018, nil, model.VideoResolutionV720p},
	}

	for _, tc := range cases {
		for _, v2 := range []bool{false, true} {
			attrs, err := ParseVideoContentWithOptions(
				model.Torrent{Name: tc.name}, classification.Result{}, ParseOptions{NoiseV2: v2},
			)
			require.NoError(t, err, tc.name)
			assert.Equal(t, tc.wantType, attrs.ContentType.ContentType, "type (v2=%v): %s", v2, tc.name)
			assert.Equal(t, tc.wantTitle, attrs.BaseTitle.String, "title (v2=%v): %s", v2, tc.name)
			assert.Equal(t, tc.wantYear, attrs.Date.Year, "year (v2=%v): %s", v2, tc.name)
			assert.Equal(t, tc.wantEpisodes.String(), attrs.Episodes.String(), "episodes (v2=%v): %s", v2, tc.name)
			assert.Equal(t, tc.wantRes, attrs.VideoResolution.VideoResolution, "resolution (v2=%v): %s", v2, tc.name)
		}
	}
}
