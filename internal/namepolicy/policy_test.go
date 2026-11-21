package namepolicy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode"

	"github.com/go-playground/validator/v10"
	"github.com/spencernorton/DH-Waypoint/internal/config"
	"github.com/spencernorton/DH-Waypoint/internal/config/configresolver"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/stretchr/testify/require"
)

func TestReleaseNamePolicyExplicitScopeAndLookalikes(t *testing.T) {
	p, err := New(Config{Enabled: true})
	require.NoError(t, err)
	for _, name := range []string{
		"Synthetic.电影.ENG.DUB.mkv",
		"Synthetic.Фильм.English.mkv",
		"Synthetic.Ж.2024.mkv",
		"Fetish.XXX.2024.mkv",
		"f.e.t.i.s.h.x-x-x.mkv",
		"FETISH+S3X.mkv",
		"Fetish\u00a0Porn.mkv",
		"Fetish.p0rn.mkv",
		"FetishXXX.mkv",
		"F.e.t.i.s.h.X.X.X.mkv",
		"FetishPorn.mkv",
		"PornXXX.mkv",
		"Porno.XXX.mkv",
		"P0rn.X.X.X.mkv",
		"Gaping.Anal.XXX.mkv",
	} {
		d := p.Evaluate(protocol.InfoHash{}, name)
		require.False(t, d.Eligible, name)
		require.Equal(t, Version, d.Version)
	}
	for _, name := range []string{
		"Café.2024.ENG.mkv",
		"České.Film.2024.mkv",
		"Shen.Ming.2024.ENG.mkv",
		"Zhui.Qin.Ai.2024.mkv",
		"Synthetic.Greek.Α.ENG.mkv",
		"Synthetic.한국어.ENG.mkv",
		"xXx.2002.1080p.mkv",
		"xXx.Return.of.Xander.Cage.mkv",
		"Fetish.2024.Documentary.mkv",
		"Sex.Education.S01.mkv",
		"Hardcore.Henry.2015.mkv",
		"Caféfetish.xxx.mkv",
		"Fetish.xxxé.mkv",
		"FetishFiction.xxx.mkv",
		"pornography.2024.mkv",
		"South.Park.Cartman.Gets.an.Anal.Probe.Season.XXX.mkv",
		"MILF.2018.adult.comedy.mkv",
		"Porno.2019.Adult.Comedy.mkv",
		"Hentai.Documentary.XXX.mkv",
		"FétishXXX.mkv",
	} {
		d := p.Evaluate(protocol.InfoHash{}, name)
		require.True(t, d.Eligible, name)
	}
	// A release name is the only string this pure gate examines; paths and
	// original-language metadata are not supplied as surrogate release names.
	require.True(t, p.Evaluate(protocol.InfoHash{}, "Allowed.Release.ENG.mkv").Eligible)
	require.False(t, p.EvaluateClassified(protocol.InfoHash{}, "Allowed.Release.mkv", "xxx").Eligible)
	require.True(t, p.EvaluateClassified(protocol.InfoHash{}, "xXx.2002.mkv", "movie").Eligible)
}

func TestDefaultOffPrivateHashConfigurationAndMissingName(t *testing.T) {
	p, err := New(NewDefaultConfig())
	require.NoError(t, err)
	require.True(t, p.Evaluate(protocol.InfoHash{}, "汉字.XXX.Fetish").Eligible)
	require.Equal(t, ReasonDisabled, p.Evaluate(protocol.InfoHash{}, "").Reason)
	h := protocol.InfoHash{1}
	p, err = New(Config{Enabled: true, ExcludedInfoHashes: []string{h.String()}})
	require.NoError(t, err)
	require.Equal(t, ReasonOwnerHash, p.Evaluate(h, "Allowed.Name").Reason)
	require.Equal(t, ReasonMissingName, p.Evaluate(protocol.InfoHash{}, " \u00a0\t").Reason)
	require.Equal(t, []protocol.InfoHash{h}, p.ExcludedHashes())
	copy := p.ExcludedHashes()
	copy[0] = protocol.InfoHash{2}
	require.Equal(t, []protocol.InfoHash{h}, p.ExcludedHashes())
}

func TestSharedScriptRangesExactlyMatchGoUnicodeMembership(t *testing.T) {
	s := specification()
	for _, pair := range []struct {
		ranges []RuneRange
		table  *unicode.RangeTable
	}{{s.Han, unicode.Han}, {s.Cyrillic, unicode.Cyrillic}} {
		for r := rune(0); r <= unicode.MaxRune; r++ {
			found := false
			for _, v := range pair.ranges {
				if r >= v.Lo && r <= v.Hi {
					found = true
					break
				}
			}
			if found != unicode.Is(pair.table, r) {
				t.Fatalf("script membership differs at U+%04X", r)
			}
		}
	}
}

func TestInternalCredentialRedactionAndFingerprintIndependence(t *testing.T) {
	secret := SecretToken(strings.Repeat("synthetic-key-", 3))
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		require.NotContains(t, fmt.Sprintf(format, secret), string(secret))
	}
	body, err := json.Marshal(Config{Enabled: true, InternalCheckToken: secret})
	require.NoError(t, err)
	require.NotContains(t, string(body), string(secret))
	a, err := New(Config{Enabled: true, InternalCheckToken: secret})
	require.NoError(t, err)
	b, err := New(Config{Enabled: true, InternalCheckToken: SecretToken(strings.Repeat("different-key-", 3))})
	require.NoError(t, err)
	require.Equal(t, a.Fingerprint(), b.Fingerprint())
	_, err = New(Config{InternalCheckToken: "short"})
	require.Error(t, err)
}

func TestResolvedCredentialRetainsValueWithoutCLIOrDebugDisclosure(t *testing.T) {
	secret := strings.Repeat("synthetic-key-", 3)
	r, err := config.New(config.Params{Specs: []config.Spec{{Key: "name_policy", DefaultValue: NewDefaultConfig()}}, Resolvers: []configresolver.Resolver{configresolver.NewEnv(map[string]string{"NAME_POLICY_INTERNAL_CHECK_TOKEN": secret})}, Validate: validator.New()})
	require.NoError(t, err)
	actual := r.Resolved.NodeMap["name_policy"].Value.(Config)
	require.Equal(t, secret, string(actual.InternalCheckToken))
	leaf := r.Resolved.NodeMap["name_policy"].ChildMap["internal_check_token"]
	require.Equal(t, "[redacted]", leaf.ValueLabel)
	body, err := json.Marshal(r.Resolved)
	require.NoError(t, err)
	require.NotContains(t, string(body), secret)
	require.NotContains(t, fmt.Sprintf("%+v", r.Resolved), secret)
}

// evaluateReference is the verbatim unoptimized specification evaluator used
// to prove 100% parity for the single-pass anchor-prefilter optimization.
func evaluateReference(p *Policy, h protocol.InfoHash, name string) Decision {
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
	if strings.TrimSpace(name) == "" {
		d.Eligible = false
		d.Reason = ReasonMissingName
		return d
	}
	for _, r := range name {
		if unicode.Is(unicode.Han, r) {
			d.Eligible = false
			d.Reason = ReasonHan
			return d
		}
	}
	for _, r := range name {
		if unicode.Is(unicode.Cyrillic, r) {
			d.Eligible = false
			d.Reason = ReasonCyrillic
			return d
		}
	}
	count, strong := 0, false
	matched := map[string]bool{}
	for i, t := range p.terms {
		if t.MatchString(name) {
			count++
			strong = strong || p.strong[i]
			matched[p.termNames[i]] = true
		}
	}
	explicitPair := false
	for _, pair := range p.explicitPairs {
		explicitPair = explicitPair || (matched[pair[0]] && matched[pair[1]])
	}
	if (count >= 2 && strong) || explicitPair {
		d.Eligible = false
		d.Reason = ReasonAdultComposite
	}
	return d
}

func TestPrefilterExactParityWithSpecification(t *testing.T) {
	p, err := New(Config{Enabled: true})
	require.NoError(t, err)
	require.LessOrEqual(t, len(p.terms), 64, "bitmask requires <= 64 terms")

	// Verify TagWhitespace matches unicode.IsSpace across the entire Unicode range.
	wsSet := make(map[rune]bool)
	for _, r := range specification().Whitespace {
		wsSet[r] = true
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if wsSet[r] != unicode.IsSpace(r) {
			t.Fatalf("TagWhitespace disagrees with unicode.IsSpace at U+%04X", r)
		}
	}

	corpus := []string{
		"", "   ", "\t\n\r\u00a0\u3000",
		"The.Matrix.1999.1080p.BluRay.x264-GROUP.mkv",
		strings.Repeat("Long.Benign.Release.Title.", 20) + "2024.1080p.mkv",
		strings.Repeat("Long.Prefix.", 25) + "F.e.t.i.s.h.X_X_X.2024.mkv",
		"Cyrillic.Фильм.and.Han.电影.mkv", // Han must take precedence over Cyrillic!
		"Han.电影.and.Cyrillic.Фильм.mkv",
		"Only.Cyrillic.Фильм.mkv",
		"Fetish.XXX.2024.mkv",
		"f.e.t.i.s.h.x-x-x.mkv",
		"F\u3000e\u00a0t_i-s+h.p\u00a00\u00a0r\u00a0n.mkv",
		"FETISH+S3X.mkv",
		"Fetish\u00a0Porn.mkv",
		"Fetish.p0rn.mkv",
		"FetishXXX.mkv",
		"FetishPorn.mkv",
		"FetishPorno.mkv",
		"PornXXX.mkv",
		"Porno.XXX.mkv",
		"PornoXXX.mkv",
		"P0rn.X.X.X.mkv",
		"pr0n.xxx.mkv",
		"Gaping.Anal.XXX.mkv",
		"g-a-p-i-n-g+a_n.a.l.hardcore.mkv",
		"Blowjob.Hentai.2024.mkv",
		"B.l.o.w.j.o.b.MILF.mkv",
		"Gangbang.NSFW.mkv",
		"Bukkake.Adult.mkv",
		"Cumshot.Sex.mkv",
		"Cum.Shot.S3x.mkv",
		"F\u00e9tish.XXX.mkv",
		"Fet\u00e9ish.XXX.mkv",
		"Caf\u00e9fetish.xxx.mkv",
		"Fetish.xxx\u00e9.mkv",
		"xXx.Return.of.Xander.Cage.2017.1080p.mkv",
		"Sex.Education.S01E01.1080p.mkv",
		"Hardcore.Henry.2015.BluRay.mkv",
		"MILF.2018.adult.comedy.mkv",
		"Porno.2019.Adult.Comedy.mkv",
		"Hentai.Documentary.XXX.mkv",
		"South.Park.Cartman.Gets.an.Anal.Probe.Season.XXX.mkv",
		"Fetish.2024.Documentary.mkv",
		"Blowjob.Single.Term.Only.mkv",
		"Cumshot.Alone.mkv",
	}
	for _, name := range corpus {
		want := evaluateReference(p, protocol.InfoHash{}, name)
		got := p.Evaluate(protocol.InfoHash{}, name)
		require.Equal(t, want, got, "mismatch for %q", name)
	}
}

func BenchmarkPolicyEvaluate(b *testing.B) {
	p, err := New(Config{Enabled: true})
	require.NoError(b, err)
	b.Run("benign_ascii", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = p.Evaluate(protocol.InfoHash{}, "The.Matrix.1999.1080p.BluRay.REMUX.TrueHD.7.1.x265-GROUP.mkv")
		}
	})
	b.Run("benign_reference", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = evaluateReference(p, protocol.InfoHash{}, "The.Matrix.1999.1080p.BluRay.REMUX.TrueHD.7.1.x265-GROUP.mkv")
		}
	})
	b.Run("adult_match", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = p.Evaluate(protocol.InfoHash{}, "F.e.t.i.s.h.X-X-X.2024.1080p.mkv")
		}
	})
}
