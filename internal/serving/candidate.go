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
	return candidateForPattern(pattern).pattern
}

func candidateForPattern(pattern string) candidate {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return candidate{}
	}
	return mandatoryCandidate(re)
}

type candidate struct {
	pattern string
	length  int
	needles []string
}

func mandatoryCandidate(re *syntax.Regexp) candidate {
	if literal, ok := candidateLiteral(re); ok {
		return candidate{pattern: literal, length: len(literal), needles: []string{literal}}
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
				best = candidate{pattern: run.String(), length: run.Len(), needles: []string{run.String()}}
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
		needles := make(map[string]struct{})
		minimum := 0
		for _, child := range re.Sub {
			next := mandatoryCandidate(child)
			if next.pattern == "" {
				return candidate{}
			}
			choices[next.pattern] = struct{}{}
			for _, needle := range next.needles {
				needles[needle] = struct{}{}
			}
			if minimum == 0 || next.length < minimum {
				minimum = next.length
			}
		}
		patterns := make([]string, 0, len(choices))
		for choice := range choices {
			patterns = append(patterns, choice)
		}
		sort.Strings(patterns)
		literals := make([]string, 0, len(needles))
		for needle := range needles {
			literals = append(literals, needle)
		}
		sort.Strings(literals)
		return candidate{pattern: "(?:" + strings.Join(patterns, "|") + ")", length: minimum, needles: literals}
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

func (p *Policy) strongMatchSQL(value, rawValue string) (string, []any) {
	if len(p.candidateNeedles) == 0 {
		return "(" + value + ") ~* ?", []any{p.strongPattern}
	}
	args := make([]any, 0, len(p.candidateNeedles)+1)
	for _, needle := range p.candidateNeedles {
		args = append(args, "%"+needle+"%")
	}
	args = append(args, p.strongPattern)
	// On ASCII UTF-8 text, a locale-independent fold plus literal contains
	// checks is cheaper than a regular expression. Every other string bypasses
	// this optional filter, preserving Unicode/locale case semantics exactly.
	// rawValue contains value (before extension removal), so it can only admit
	// extra candidates and avoids computing the base path for ordinary files.
	// CASE retains the authoritative matcher and its explicit short circuit.
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(p.candidateNeedles)), ",")
	return "CASE WHEN getdatabaseencoding() <> 'UTF8' OR octet_length(" + rawValue + ") <> char_length(" + rawValue +
		") OR translate(" + rawValue + ", 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz') LIKE ANY(ARRAY[" + placeholders +
		"]) THEN (" + value + ") ~* ? ELSE FALSE END", args
}
