package serving

import (
	"math/rand"
	"regexp"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/keywords"
	"github.com/stretchr/testify/require"
)

func TestCandidateNeverRejectsACompleteKeywordMatch(t *testing.T) {
	groups := [][]string{
		{"siterip", "private ?gold", "handjobs?", "*gloryhole*"},
		{"(red|blue) ?studio", "a(b|c)d", "ab+c"},
		{"café", "東京", "plain"},
		{"(hot)?"},
	}
	words := []string{"siterip", "SITERIP", "privategold", "private.gold", "handjob", "handjobs",
		"gloryhole", "mygloryholes", "redstudio", "blue.studio", "abd", "acd", "abbbc", "café", "東京", "plain", "hot", ""}
	for _, group := range groups {
		full, err := keywords.NewRegexFromKeywords(group...)
		require.NoError(t, err)
		pattern := candidatePattern(full.String())
		if pattern == "" {
			continue // Unsupported/unconstrained alternatives keep the original path.
		}
		coarse, err := regexp.Compile("(?i)" + pattern)
		require.NoError(t, err)
		for _, word := range words {
			for _, prefix := range []string{"", "/", "film.", "π", "movie/"} {
				for _, suffix := range []string{"", ".mkv", "東京", "-2026"} {
					text := prefix + word + suffix
					if full.MatchString(text) {
						require.True(t, coarse.MatchString(text), "%q: necessary candidate %q rejected %q", group, pattern, text)
					}
				}
			}
		}
		rng := rand.New(rand.NewSource(17)) // Deterministic invented filename noise.
		alphabet := []rune("abcdehilmnoprstu.-_/0123π東京")
		for range 5000 {
			text := make([]rune, rng.Intn(48))
			for i := range text {
				text[i] = alphabet[rng.Intn(len(alphabet))]
			}
			if full.MatchString(string(text)) {
				require.True(t, coarse.MatchString(string(text)))
			}
		}
	}
}

func TestUnconstrainedAlternativesDisableTheCandidate(t *testing.T) {
	for _, pattern := range []string{"", "a?", "a|", "(?:a|東京)", ".*", "[a-z]+", "("} {
		require.Empty(t, candidatePattern(pattern), pattern)
	}
}

func TestCandidateRetainsTheOriginalMatcherBehindAShortCircuit(t *testing.T) {
	p, err := NewPolicy(Config{ExcludeAdult: true}, []string{"private ?gold", "siterip"}, []string{"mkv"})
	require.NoError(t, err)
	require.NotEmpty(t, p.candidatePattern)
	sql, args := p.strongMatchSQL("name")
	require.Contains(t, sql, "CASE WHEN")
	require.Contains(t, sql, "ELSE FALSE END")
	require.Equal(t, []any{p.candidatePattern, p.strongPattern}, args)
	p.candidatePattern = ""
	sql, args = p.strongMatchSQL("name")
	require.Equal(t, "(name) ~* ?", sql)
	require.Equal(t, []any{p.strongPattern}, args)
}
