package serving

import (
	"regexp/syntax"
	"sort"
	"strings"
)

// candidatePattern selects a necessary ASCII substring from each alternative.
// It may admit extra strings; the original Unicode-aware expression remains
// authoritative. An optional or unsupported branch disables the prefilter.
func candidatePattern(pattern string) string {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return ""
	}
	return mandatoryCandidate(re).pattern
}

type candidate struct {
	pattern string
	length  int
}

func mandatoryCandidate(re *syntax.Regexp) candidate {
	if literal, ok := candidateLiteral(re); ok {
		return candidate{literal, len(literal)}
	}
	switch re.Op {
	case syntax.OpCapture, syntax.OpPlus:
		return mandatoryCandidate(re.Sub[0])
	case syntax.OpRepeat:
		if re.Min > 0 {
			return mandatoryCandidate(re.Sub[0])
		}
	case syntax.OpConcat:
		var best candidate
		var run strings.Builder
		flush := func() {
			if run.Len() > best.length {
				best = candidate{run.String(), run.Len()}
			}
			run.Reset()
		}
		for _, child := range re.Sub {
			if literal, ok := candidateLiteral(child); ok {
				run.WriteString(literal)
				continue
			}
			flush()
			if next := mandatoryCandidate(child); next.length > best.length {
				best = next
			}
		}
		flush()
		return best
	case syntax.OpAlternate:
		choices := make(map[string]struct{})
		minimum := 0
		for _, child := range re.Sub {
			next := mandatoryCandidate(child)
			if next.pattern == "" {
				return candidate{}
			}
			choices[next.pattern] = struct{}{}
			if minimum == 0 || next.length < minimum {
				minimum = next.length
			}
		}
		patterns := make([]string, 0, len(choices))
		for choice := range choices {
			patterns = append(patterns, choice)
		}
		sort.Strings(patterns)
		return candidate{"(?:" + strings.Join(patterns, "|") + ")", minimum}
	}
	return candidate{}
}

// Only fixed ASCII alphanumeric runs are eligible. In particular, a Unicode
// range, wildcard, optional group, or digit class is never guessed to be text.
func candidateLiteral(re *syntax.Regexp) (string, bool) {
	lower := func(r rune) (rune, bool) {
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		return r, r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
	}
	if re.Op == syntax.OpLiteral {
		var out strings.Builder
		for _, r := range re.Rune {
			value, ok := lower(r)
			if !ok {
				return "", false
			}
			out.WriteRune(value)
		}
		return out.String(), out.Len() > 0
	}
	if re.Op == syntax.OpCharClass {
		var single rune
		for i := 0; i < len(re.Rune); i += 2 {
			if re.Rune[i] != re.Rune[i+1] {
				return "", false
			}
			value, ok := lower(re.Rune[i])
			if !ok || single != 0 && single != value {
				return "", false
			}
			single = value
		}
		if single != 0 {
			return string(single), true
		}
	}
	return "", false
}

func (p *Policy) strongMatchSQL(value string) (string, []any) {
	if p.candidatePattern == "" {
		return "(" + value + ") ~* ?", []any{p.strongPattern}
	}
	// CASE makes short-circuiting explicit even if the planner reorders WHERE
	// qualifications. This changes evaluation cost, never the final matcher.
	return "CASE WHEN (" + value + ") ~* ? THEN (" + value + ") ~* ? ELSE FALSE END",
		[]any{p.candidatePattern, p.strongPattern}
}
