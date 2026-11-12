package classifier

import (
	"context"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/tmdb"
)

type searchEpisodesCtxKey struct{}

func withSearchEpisodes(ctx context.Context, episodes model.Episodes) context.Context {
	if len(episodes) == 0 {
		return ctx
	}
	return context.WithValue(ctx, searchEpisodesCtxKey{}, episodes)
}

func searchEpisodesFromContext(ctx context.Context) model.Episodes {
	if ctx == nil {
		return nil
	}
	eps, _ := ctx.Value(searchEpisodesCtxKey{}).(model.Episodes)
	return eps
}

func hasPositiveSeasonOrEpisode(episodes model.Episodes) bool {
	for season, epMap := range episodes {
		if season > 0 || len(epMap) > 0 {
			return true
		}
	}
	return false
}

// contentEpisodeFeasibility checks whether a TV show's stored TMDB season and
// episode counts can accommodate the torrent's parsed season/episode claim.
//
// strictOK requires maxSeason <= number_of_seasons and every claimed episode
// number <= season_episodes:<S> (when present).
//
// localGraceOK allows maxSeason == number_of_seasons + 1 so a locally cached
// returning show whose TMDB metadata has not yet refreshed for a new season is
// not unnecessarily discarded, while still rejecting gross homonym mismatches
// (such as Season 11 against a 1-season show).
func contentEpisodeFeasibility(content model.Content, episodes model.Episodes) (strictOK bool, localGraceOK bool) {
	if len(episodes) == 0 {
		return true, true
	}

	numSeasons := 0
	seasonEpCounts := make(map[int]int)
	for _, attr := range content.Attributes {
		if attr.Source != model.SourceTmdb {
			continue
		}
		switch {
		case attr.Key == tmdb.AttrKeyNumberOfSeasons:
			if v, err := strconv.Atoi(attr.Value); err == nil && v > 0 {
				numSeasons = v
			}
		case strings.HasPrefix(attr.Key, tmdb.AttrKeySeasonEpisodesPrefix):
			sNumStr := strings.TrimPrefix(attr.Key, tmdb.AttrKeySeasonEpisodesPrefix)
			sNum, errS := strconv.Atoi(sNumStr)
			epCnt, errE := strconv.Atoi(attr.Value)
			if errS == nil && errE == nil && sNum > 0 && epCnt > 0 {
				seasonEpCounts[sNum] = epCnt
			}
		}
	}

	if numSeasons <= 0 && len(seasonEpCounts) == 0 {
		return true, true
	}

	maxSeason := 0
	maxEpBySeason := make(map[int]int, len(episodes))
	for season, epMap := range episodes {
		if season > maxSeason {
			maxSeason = season
		}
		maxEp := 0
		for ep := range epMap {
			if ep > maxEp {
				maxEp = ep
			}
		}
		if season > 0 && maxEp > 0 {
			maxEpBySeason[season] = maxEp
		}
	}

	strictOK = true
	localGraceOK = true

	if numSeasons > 0 {
		if maxSeason > numSeasons {
			strictOK = false
		}
		if maxSeason > numSeasons+1 {
			localGraceOK = false
		}
	}

	for season, maxEp := range maxEpBySeason {
		epCnt, ok := seasonEpCounts[season]
		if !ok || epCnt <= 0 {
			continue
		}
		if maxEp > epCnt {
			strictOK = false
			// Prior seasons (and single-season shows where maxEp far exceeds
			// the season run) have fixed episode counts.
			if (numSeasons > 0 && season < numSeasons) || maxEp > epCnt+2 {
				localGraceOK = false
			}
		}
	}

	return strictOK, localGraceOK
}

const minPrimaryEpisodeFileBytes = 20 * 1024 * 1024 // 20 MiB

var primaryVideoExts = map[string]struct{}{
	"mkv": {}, "mp4": {}, "avi": {}, "m4v": {}, "ts": {}, "m2ts": {},
	"webm": {}, "mov": {}, "wmv": {}, "mpg": {}, "mpeg": {}, "flv": {},
	"ogm": {}, "vob": {},
}

var (
	sampleFileTokenRe       = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(sample|trailer|featurette|extras?)(?:[^a-z0-9]|$)`)
	explicitSeasonPackRe    = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(complete|full[ ._-]*season|season[ ._-]*pack|season[ ._-]*[0-9]{1,2})(?:[^a-z0-9]|$)`)
	explicitEpisodeOrPartRe = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(ep(?:isode)?[ ._-]*[0-9]{1,3}|part[ ._-]*[0-9]{1,2}|pt[ ._-]*[0-9]{1,2}|e[0-9]{1,3})(?:[^a-z0-9]|$)`)
)

func countPrimaryVideoFiles(files []model.TorrentFile) int {
	count := 0
	for _, f := range files {
		if f.Size < minPrimaryEpisodeFileBytes {
			continue
		}
		ext := strings.ToLower(strings.TrimPrefix(f.Extension.String, "."))
		if !f.Extension.Valid || ext == "" {
			ext = strings.ToLower(strings.TrimPrefix(filepath.Ext(f.Path), "."))
		}
		if _, ok := primaryVideoExts[ext]; !ok {
			continue
		}
		if f.Size < 64*1024*1024 && sampleFileTokenRe.MatchString(f.Path) {
			continue
		}
		count++
	}
	return count
}

func hasExplicitSeasonPackPhrase(name string) bool {
	if !explicitSeasonPackRe.MatchString(name) {
		return false
	}
	return !explicitEpisodeOrPartRe.MatchString(name)
}

// shouldDemoteSmallSeasonOnly reports whether a season-only parse (e.g. {2:{}})
// below ceiling should be cleared as a suspected single episode with
// unrecognised episode notation. Confirmed multi-season packs, multi-video-file
// torrents, and non-single-file torrents explicitly titled as season packs keep
// their season claim even when compactly encoded below ceiling (<= 4 GiB).
func shouldDemoteSmallSeasonOnly(t model.Torrent, episodes model.Episodes, ceiling int64) bool {
	if ceiling <= 0 || !isSeasonOnlyPack(episodes) {
		return false
	}
	if int64(t.Size) <= 0 || int64(t.Size) > ceiling {
		return false
	}
	if len(episodes) >= 2 {
		return false
	}
	if countPrimaryVideoFiles(t.Files) >= 2 {
		return false
	}
	if t.FilesStatus != model.FilesStatusSingle && hasExplicitSeasonPackPhrase(t.Name) {
		return false
	}
	return true
}
