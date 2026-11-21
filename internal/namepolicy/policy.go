// Package namepolicy makes a pure library-eligibility decision from the
// authoritative release name. It does not infer media tracks, origin or death,
// dispatch work, authorize public grabs or mutate catalogue/blocking state.
package namepolicy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spencernorton/DH-Waypoint/internal/catalogueguard"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/telemetry/dualemit"
)

const Version = "release-name-policy-v1"

const (
	ReasonDisabled       = "disabled"
	ReasonAllowed        = "allowed"
	ReasonOwnerHash      = "owner_excluded_hash"
	ReasonMissingName    = "name_unavailable"
	ReasonHan            = "han_release_name"
	ReasonCyrillic       = "cyrillic_release_name"
	ReasonAdultComposite = "explicit_adult_name"
	ReasonAdultType      = "explicit_adult_classification"
	ReasonNotServed      = "not_served"
)

var ErrExcluded = errors.New("release excluded by name policy")

type Decision struct {
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason"`
	Version  string `json:"version"`
}

type Policy struct {
	enabled             bool
	excluded            map[protocol.InfoHash]struct{}
	excludedHashes      []protocol.InfoHash
	internalToken       SecretToken
	terms               []*regexp.Regexp
	strong              []bool
	termNames           []string
	explicitPairs       [][2]string
	explicitPairIndices [][2]int
	anchorNeedles       [][]byte
	denied              *dualemit.CounterVec
}

// anchorNeedlesForSpec returns sound lowercased ASCII byte needles: at least
// one needle must appear in the separator-stripped ASCII-lowercased release
// name for any strong term or explicit adult pair to match. Because every
// termPattern only allows ASCII letters/digits interleaved with separators
// (TagWhitespace + "._-+"), absence of every needle guarantees ReasonAllowed
// without running the full regex set.
var defaultAnchorNeedles = [][]byte{
	[]byte("fetish"),
	[]byte("blowjob"),
	[]byte("gangbang"),
	[]byte("bukkake"),
	[]byte("cumshot"),
	[]byte("gapinganal"),
	[]byte("xxx"),
}

func New(c Config) (*Policy, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	p := &Policy{
		enabled:       c.Enabled,
		excluded:      map[protocol.InfoHash]struct{}{},
		internalToken: c.InternalCheckToken,
		anchorNeedles: defaultAnchorNeedles,
		denied:        newDeniedMetric(),
	}
	for _, value := range c.ExcludedInfoHashes {
		h, _ := protocol.ParseInfoHash(value)
		p.excluded[h] = struct{}{}
	}
	for h := range p.excluded {
		p.excludedHashes = append(p.excludedHashes, h)
	}
	sort.Slice(p.excludedHashes, func(i, j int) bool { return p.excludedHashes[i].String() < p.excludedHashes[j].String() })
	spec := specification()
	p.explicitPairs = spec.ExplicitAdultPairs
	nameToIndex := make(map[string]int, len(spec.AdultTerms))
	for i, t := range spec.AdultTerms {
		p.terms = append(p.terms, regexp.MustCompile(t.Pattern))
		p.strong = append(p.strong, t.Strong)
		p.termNames = append(p.termNames, t.Name)
		nameToIndex[t.Name] = i
	}
	for _, pair := range spec.ExplicitAdultPairs {
		a, okA := nameToIndex[pair[0]]
		b, okB := nameToIndex[pair[1]]
		if okA && okB {
			p.explicitPairIndices = append(p.explicitPairIndices, [2]int{a, b})
		}
	}
	return p, nil
}

func (p *Policy) Enabled() bool { return p != nil && p.enabled }
func (p *Policy) ExcludedHashes() []protocol.InfoHash {
	if p == nil {
		return nil
	}
	return append([]protocol.InfoHash(nil), p.excludedHashes...)
}
func (p *Policy) ExcludesHash(h protocol.InfoHash) bool {
	if !p.Enabled() {
		return false
	}
	_, ok := p.excluded[h]
	return ok
}
func (p *Policy) InternalToken() SecretToken {
	if p == nil {
		return ""
	}
	return p.internalToken
}

// Fingerprint binds the decision configuration, excluding the internal API
// credential. Secret rotation cannot change model/application policy identity.
func (p *Policy) Fingerprint() string {
	body, _ := json.Marshal(struct {
		Definition Specification
		Enabled    bool
		Hashes     []protocol.InfoHash
	}{specification(), p.Enabled(), p.ExcludedHashes()})
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

func isASCIISpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

func isASCIISeparator(b byte) bool {
	return isASCIISpace(b) || b == '.' || b == '_' || b == '-' || b == '+'
}

// prefilterAndCheckScripts scans name once to enforce missing-name, Han, and
// Cyrillic rules (preserving exact Han-before-Cyrillic precedence) while
// building a separator-stripped ASCII-lowercased buffer on the stack for
// zero-allocation anchor prefiltering.
func prefilterAndCheckScripts(name string, stackBuf *[256]byte) (stopReason string, stripped []byte) {
	buf := stackBuf[:0]
	if len(name) > len(stackBuf) {
		buf = make([]byte, 0, len(name))
	}
	nonSpace := false
	hasCyrillic := false
	for i := 0; i < len(name); {
		b := name[i]
		if b < utf8.RuneSelf {
			if !isASCIISpace(b) {
				nonSpace = true
			}
			if !isASCIISeparator(b) {
				if b >= 'A' && b <= 'Z' {
					b += 'a' - 'A'
				}
				buf = append(buf, b)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(name[i:])
		i += size
		if unicode.Is(unicode.Han, r) {
			return ReasonHan, nil
		}
		if unicode.Is(unicode.Cyrillic, r) {
			hasCyrillic = true
		}
		if !unicode.IsSpace(r) {
			nonSpace = true
			// Non-ASCII non-separator rune cannot appear inside any ASCII term
			// word; emit a NUL barrier so surrounding letters never coalesce.
			buf = append(buf, 0)
		}
	}
	if !nonSpace {
		return ReasonMissingName, nil
	}
	if hasCyrillic {
		return ReasonCyrillic, nil
	}
	return "", buf
}

func (p *Policy) hasAnchorNeedle(stripped []byte) bool {
	for _, needle := range p.anchorNeedles {
		if bytes.Contains(stripped, needle) {
			return true
		}
	}
	return false
}

func (p *Policy) Evaluate(h protocol.InfoHash, name string) Decision {
	d := Decision{Eligible: true, Reason: ReasonAllowed, Version: Version}
	if !p.Enabled() {
		d.Reason = ReasonDisabled
		return d
	}
	if p.ExcludesHash(h) {
		d.Eligible = false
		d.Reason = ReasonOwnerHash
		return d
	}
	var stackBuf [256]byte
	stopReason, stripped := prefilterAndCheckScripts(name, &stackBuf)
	if stopReason != "" {
		d.Eligible = false
		d.Reason = stopReason
		return d
	}
	if !p.hasAnchorNeedle(stripped) {
		return d
	}
	count, strong := 0, false
	var matchedMask uint64
	for i, t := range p.terms {
		if t.MatchString(name) {
			count++
			strong = strong || p.strong[i]
			matchedMask |= 1 << uint(i)
		}
	}
	explicitPair := false
	for _, pair := range p.explicitPairIndices {
		bitA := uint64(1) << uint(pair[0])
		bitB := uint64(1) << uint(pair[1])
		if matchedMask&bitA != 0 && matchedMask&bitB != 0 {
			explicitPair = true
			break
		}
	}
	if (count >= 2 && strong) || explicitPair {
		d.Eligible = false
		d.Reason = ReasonAdultComposite
	}
	return d
}

// EvaluateClassified also uses an already known explicit adult classification;
// it does not run a classifier or treat title/metadata origin as that fact.
func (p *Policy) EvaluateClassified(h protocol.InfoHash, name, contentType string) Decision {
	d := p.Evaluate(h, name)
	if p.Enabled() && d.Eligible && contentType == "xxx" {
		d.Eligible = false
		d.Reason = ReasonAdultType
	}
	return d
}

type RuneRange struct{ Lo, Hi rune }
type Term struct {
	Name, Pattern string
	Strong        bool
}
type Specification struct {
	Version                   string
	UnicodeVersion            string
	Whitespace                string
	Han, Cyrillic             []RuneRange
	AdultTerms                []Term
	MinimumDistinctAdultTerms int
	ExplicitAdultPairs        [][2]string
}

func (p *Policy) Specification() Specification { return specification() }

func scriptRanges(table *unicode.RangeTable) []RuneRange {
	var out []RuneRange
	add := func(lo, hi, stride rune) {
		if stride == 1 {
			out = append(out, RuneRange{lo, hi})
			return
		}
		for r := lo; r <= hi; r += stride {
			out = append(out, RuneRange{r, r})
		}
	}
	for _, r := range table.R16 {
		add(rune(r.Lo), rune(r.Hi), rune(r.Stride))
	}
	for _, r := range table.R32 {
		add(rune(r.Lo), rune(r.Hi), rune(r.Stride))
	}
	return out
}

// Explicit release delimiters preserve accented/other letter lookalikes. The
// ASCII case classes and regex syntax are shared directly with SQL rendering;
// no Unicode case folding or origin/file-path inference is used.
func termPattern(words ...string) string {
	delimiters := strings.ReplaceAll(regexp.QuoteMeta(catalogueguard.TagWhitespace+`._-+:/\()[]{}!,;=?&'"|#%@`), "-", `\-`)
	separators := strings.ReplaceAll(regexp.QuoteMeta(catalogueguard.TagWhitespace+`._-+`), "-", `\-`)
	var variants []string
	for _, w := range words {
		var letters []string
		for _, r := range w {
			if r >= 'a' && r <= 'z' {
				letters = append(letters, "["+string(r)+string(r-'a'+'A')+"]")
			} else {
				letters = append(letters, regexp.QuoteMeta(string(r)))
			}
		}
		variants = append(variants, strings.Join(letters, "["+separators+"]*"))
	}
	return "(^|[" + delimiters + "])(" + strings.Join(variants, "|") + ")($|[" + delimiters + "])"
}

func specification() Specification {
	s := Specification{Version: Version, UnicodeVersion: unicode.Version, Whitespace: catalogueguard.TagWhitespace, Han: scriptRanges(unicode.Han), Cyrillic: scriptRanges(unicode.Cyrillic), MinimumDistinctAdultTerms: 2, ExplicitAdultPairs: [][2]string{{"porn", "xxx"}, {"porno", "xxx"}}}
	// Fetish is the owner's explicit anchor; the other strong anchors are
	// acts from the existing precision-qualified tier. Anal/MILF/Porno remain
	// ambiguous title vocabulary and cannot supply strong evidence alone.
	s.AdultTerms = append(s.AdultTerms, Term{"fetish", termPattern("fetish", "fetishxxx", "fetishporn", "fetishporno"), true})
	for _, w := range []string{"blowjob", "gangbang", "bukkake", "cumshot"} {
		s.AdultTerms = append(s.AdultTerms, Term{w, termPattern(w), true})
	}
	s.AdultTerms = append(s.AdultTerms, Term{"gaping_anal", termPattern("gapinganal"), true})
	s.AdultTerms = append(s.AdultTerms, Term{"porn", termPattern("porn", "p0rn", "pr0n", "fetishporn", "pornxxx"), false})
	s.AdultTerms = append(s.AdultTerms, Term{"porno", termPattern("porno", "fetishporno", "pornoxxx"), false})
	s.AdultTerms = append(s.AdultTerms, Term{"xxx", termPattern("xxx", "fetishxxx", "pornxxx", "pornoxxx"), false})
	for _, w := range []string{"hentai", "milf", "anal", "adult", "nsfw", "hardcore"} {
		s.AdultTerms = append(s.AdultTerms, Term{w, termPattern(w), false})
	}
	s.AdultTerms = append(s.AdultTerms, Term{"sex", termPattern("sex", "s3x"), false})
	return s
}
