package leanmode

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/spencernorton/DH-Waypoint/internal/model"
)

// SlotKind identifies the granularity class of a slot for size-limit and pack rules.
type SlotKind string

const (
	SlotKindMovie        SlotKind = "movie"
	SlotKindEpisode      SlotKind = "episode"
	SlotKindMultiEpisode SlotKind = "multi_episode"
	SlotKindSeasonPack   SlotKind = "season_pack"
	SlotKindSeriesPack   SlotKind = "series_pack"
)

// SlotAssignment maps one candidate release to its canonical slot key and metadata.
type SlotAssignment struct {
	SlotKey  string
	Kind     SlotKind
	Season   int
	Episodes []int
	IsPack   bool
}

// DeriveSlot computes the canonical slot key for a linked movie or TV candidate.
// Returns (SlotAssignment{}, false) for unlinked or out-of-scope releases.
func DeriveSlot(c Candidate, scope ScopeConfig) (SlotAssignment, bool) {
	if c.ContentID == "" || c.ContentSource == "" {
		return SlotAssignment{}, false
	}
	if !slices.Contains(scope.ContentTypes, c.ContentType) {
		return SlotAssignment{}, false
	}
	base := c.ContentType + ":" + c.ContentSource + ":" + c.ContentID
	if c.ContentType == "movie" {
		return SlotAssignment{
			SlotKey: base,
			Kind:    SlotKindMovie,
		}, true
	}
	if c.ContentType != "tv_show" {
		return SlotAssignment{}, false
	}

	// Derive granularity when absent, using the single model source of truth.
	gran := c.ReleaseGranularity
	if gran == "" {
		derived := model.DeriveReleaseGranularity(
			model.NewNullContentType(model.ContentTypeTvShow),
			c.Episodes,
			c.Name,
		)
		if derived.Valid {
			gran = derived.ReleaseGranularity.String()
		}
	}

	seasons := sortedSeasons(c.Episodes)
	switch {
	case gran == model.ReleaseGranularityCompleteSeries.String() || len(seasons) > 1:
		if len(seasons) == 0 {
			return SlotAssignment{
				SlotKey: base + ":SERIES",
				Kind:    SlotKindSeriesPack,
				IsPack:  true,
			}, true
		}
		return SlotAssignment{
			SlotKey: fmt.Sprintf("%s:S%02d-S%02d", base, seasons[0], seasons[len(seasons)-1]),
			Kind:    SlotKindSeriesPack,
			Season:  seasons[0],
			IsPack:  true,
		}, true

	case len(seasons) == 1:
		s := seasons[0]
		eps := sortedEpisodes(c.Episodes[s])
		switch {
		case len(eps) == 0 || gran == model.ReleaseGranularitySeason.String() || gran == model.ReleaseGranularityPartialSeason.String():
			return SlotAssignment{
				SlotKey:  fmt.Sprintf("%s:S%02d", base, s),
				Kind:     SlotKindSeasonPack,
				Season:   s,
				Episodes: eps,
				IsPack:   true,
			}, true
		case len(eps) == 1:
			return SlotAssignment{
				SlotKey:  fmt.Sprintf("%s:S%02dE%02d", base, s, eps[0]),
				Kind:     SlotKindEpisode,
				Season:   s,
				Episodes: eps,
			}, true
		default:
			parts := make([]string, len(eps))
			for i, e := range eps {
				parts[i] = fmt.Sprintf("E%02d", e)
			}
			return SlotAssignment{
				SlotKey:  fmt.Sprintf("%s:S%02d%s", base, s, strings.Join(parts, "+")),
				Kind:     SlotKindMultiEpisode,
				Season:   s,
				Episodes: eps,
			}, true
		}

	default:
		// TV release with valid content_id but no parsed season/episode markers:
		// group under an unslotted bucket so we never falsely merge unrelated episodes.
		if c.AnimeAbsoluteEpisode > 0 {
			return SlotAssignment{
				SlotKey:  fmt.Sprintf("%s:ABS%04d", base, c.AnimeAbsoluteEpisode),
				Kind:     SlotKindEpisode,
				Episodes: []int{c.AnimeAbsoluteEpisode},
			}, true
		}
		return SlotAssignment{
			SlotKey: base + ":UNSLOTTED",
			Kind:    SlotKindSeasonPack,
			IsPack:  true,
		}, true
	}
}

func sortedSeasons(eps model.Episodes) []int {
	if len(eps) == 0 {
		return nil
	}
	out := make([]int, 0, len(eps))
	for s := range eps {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func sortedEpisodes(m map[int]struct{}) []int {
	if len(m) == 0 {
		return nil
	}
	out := make([]int, 0, len(m))
	for e := range m {
		out = append(out, e)
	}
	slices.Sort(out)
	return out
}

// ParseSeasonFromSlotKey extracts the integer season from a "tv_show:src:id:S01..." slot key.
func ParseSeasonFromSlotKey(slotKey string) (int, bool) {
	idx := strings.LastIndex(slotKey, ":S")
	if idx < 0 || idx+2 >= len(slotKey) {
		return 0, false
	}
	rest := slotKey[idx+2:]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}
