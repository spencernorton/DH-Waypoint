package classifier

import (
	"sort"

	"github.com/agnivade/levenshtein"
	"github.com/mozillazg/go-unidecode"
	"github.com/spencernorton/DH-Waypoint/internal/regex"
	"github.com/spencernorton/DH-Waypoint/internal/titlenorm"
)

const levenshteinThreshold = 5

type candidateTieBreak struct {
	Popularity float32
	VoteCount  int64
}

type scoredMatchCandidate struct {
	idx        int
	levDist    int
	tsr        float64
	penalty    int
	popularity float32
	voteCount  int64
}

func betterScoredCandidate(a, b scoredMatchCandidate) bool {
	// Distant year mismatches (penalty >= 100) always lose to compatible years,
	// while within compatible years (penalty <= 1) an exact normalized title
	// (levDist == 0) beats a non-exact edit-distance match.
	aCompat := a.penalty <= 1
	bCompat := b.penalty <= 1
	if aCompat != bCompat {
		return aCompat
	}
	if aCompat && bCompat {
		aExact := a.levDist == 0
		bExact := b.levDist == 0
		if aExact != bExact {
			return aExact
		}
	}
	if a.penalty != b.penalty {
		return a.penalty < b.penalty
	}
	if a.levDist != b.levDist {
		return a.levDist < b.levDist
	}
	if a.tsr != b.tsr {
		return a.tsr > b.tsr
	}
	if a.popularity != b.popularity {
		return a.popularity > b.popularity
	}
	if a.voteCount != b.voteCount {
		return a.voteCount > b.voteCount
	}
	return a.idx < b.idx
}

func rankMatchCandidates[T any](
	target string,
	items []T,
	getCandidates func(T) []string,
	getPenalty func(T) int,
	getTieBreak func(T) candidateTieBreak,
	fuzzyMatchEnabled bool,
) []T {
	if len(items) == 0 {
		return nil
	}

	targetIsJP := titlenorm.ContainsJapaneseScript(target)
	targetNative := ""
	if targetIsJP {
		targetNative = nativeMatchKey(target)
	}

	var normTarget string
	if fuzzyMatchEnabled {
		normTarget = normalizeTitleForMatch(foldTitlePunct(target))
	} else {
		normTarget = levenshteinNormalizeString(foldTitlePunct(target))
	}
	threshold := fuzzyLevThreshold(normTarget)
	targetFirstTok := firstToken(normTarget)

	var scored []scoredMatchCandidate
	for i, item := range items {
		candidates := getCandidates(item)
		penalty := 0
		if getPenalty != nil {
			penalty = getPenalty(item)
		}
		var tb candidateTieBreak
		if getTieBreak != nil {
			tb = getTieBreak(item)
		}

		var itemBest *scoredMatchCandidate
		consider := func(c scoredMatchCandidate) {
			if itemBest == nil || betterScoredCandidate(c, *itemBest) {
				copyCand := c
				itemBest = &copyCand
			}
		}

		for _, raw := range candidates {
			if targetNative != "" && nativeMatchKey(raw) == targetNative {
				consider(scoredMatchCandidate{
					idx: i, levDist: 0, tsr: 1.0, penalty: penalty,
					popularity: tb.Popularity, voteCount: tb.VoteCount,
				})
				continue
			}

			var normCand string
			if fuzzyMatchEnabled {
				normCand = normalizeTitleForMatch(foldTitlePunct(raw))
			} else {
				normCand = levenshteinNormalizeString(foldTitlePunct(raw))
			}
			if normCand == "" {
				continue
			}

			if !targetIsJP && firstToken(normCand) != targetFirstTok {
				continue
			}

			levDist := levenshtein.ComputeDistance(normTarget, normCand)
			tsr := tokenSetRatio(normTarget, normCand)
			accepted := levDist <= threshold || (fuzzyMatchEnabled && tsr >= 0.90)
			if !accepted {
				continue
			}

			consider(scoredMatchCandidate{
				idx: i, levDist: levDist, tsr: tsr, penalty: penalty,
				popularity: tb.Popularity, voteCount: tb.VoteCount,
			})
		}

		if itemBest != nil {
			scored = append(scored, *itemBest)
		}
	}

	sort.Slice(scored, func(i, j int) bool {
		return betterScoredCandidate(scored[i], scored[j])
	})

	out := make([]T, len(scored))
	for i, s := range scored {
		out[i] = items[s.idx]
	}
	return out
}

func levenshteinFindBestMatch[T any](target string, items []T, getCandidates func(T) []string) (t T, ok bool) {
	ranked := rankMatchCandidates(target, items, getCandidates, nil, nil, false)
	if len(ranked) == 0 {
		return t, false
	}
	return ranked[0], true
}

func fuzzyFindBestMatch[T any](
	target string,
	items []T,
	getCandidates func(T) []string,
	getYearPenalty func(T) int,
	fuzzyMatchEnabled bool,
) (t T, ok bool) {
	ranked := rankMatchCandidates(target, items, getCandidates, getYearPenalty, nil, fuzzyMatchEnabled)
	if len(ranked) == 0 {
		return t, false
	}
	return ranked[0], true
}

func levenshteinFindMinDistance(target string, candidates []string) int {
	targetIsJP := titlenorm.ContainsJapaneseScript(target)
	targetNative := ""
	if targetIsJP {
		targetNative = nativeMatchKey(target)
	}
	normTarget := levenshteinNormalizeString(foldTitlePunct(target))
	threshold := fuzzyLevThreshold(normTarget)
	targetFirstTok := firstToken(normTarget)
	triedCandidates := make(map[string]struct{}, len(candidates))
	minDistance := -1

	for _, candidate := range candidates {
		if targetNative != "" && nativeMatchKey(candidate) == targetNative {
			return 0
		}
		normCandidate := levenshteinNormalizeString(foldTitlePunct(candidate))
		if normCandidate == "" {
			continue
		}
		if _, ok := triedCandidates[normCandidate]; ok {
			continue
		}
		triedCandidates[normCandidate] = struct{}{}

		if !targetIsJP && firstToken(normCandidate) != targetFirstTok {
			continue
		}

		distance := levenshtein.ComputeDistance(normTarget, normCandidate)
		if distance > threshold {
			continue
		}
		if minDistance == -1 || distance < minDistance {
			minDistance = distance
		}
	}

	return minDistance
}

func levenshteinNormalizeString(str string) string {
	return regex.NormalizeString(unidecode.Unidecode(str))
}
