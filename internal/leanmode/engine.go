package leanmode

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/model"
)

// Candidate is the normalized, DB-independent release snapshot evaluated by the
// three-tier LEAN MODE engine.
type Candidate struct {
	InfoHash               []byte
	Name                   string
	ContentType            string
	ContentSource          string
	ContentID              string
	SizeBytes              uint64
	FilesStatus            string
	FilesCount             int
	VideoFileCount         int
	HasEmbeddedSubtitles   bool
	HasClutterFiles        bool
	VideoResolution        string // e.g. "1080p", "2160p"
	VideoSource            string // e.g. "BluRay", "WEBDL", "WEBRip", "CAM"
	VideoCodec             string // e.g. "x265", "HEVC", "H264", "x264", "AV1"
	VideoModifier          string // e.g. "REMUX", "SCREENER"
	Video3D                string
	ReleaseGroup           string
	Languages              []string
	EnglishAudio           *bool
	Episodes               model.Episodes
	ReleaseGranularity     string
	AnimeAbsoluteEpisode   int
	HDRFormats             []string
	AudioFormats           []string
	AudioFeatures          []string
	Revisions              []string
	Seeders                *int // nil = unknown; 0 = observed zero (or tracker zero)
	SeedersAuthoritative   bool // true when backed by tracker scrape
	ArmigerApproved        bool
	PayloadProofActive     bool
	PayloadProofVerifiedAt *time.Time
	ArmigerEvaluatedAt     *time.Time
	PublishedAt            time.Time
	CreatedAt              time.Time
	LivenessDead           bool
	Protected              bool // private, canonical_label, label_evidence, grab_attempt, hint, protected tag, hold, quarantine
}

// IncumbentState represents the current rank-1 winner of a slot from lean_winners.
type IncumbentState struct {
	InfoHash       []byte
	PreferredScore float64
	TiebreakScore  float64
	IncumbentSince time.Time
}

// ScoredCandidate carries full three-tier scoring provenance for explainability and ranking.
type ScoredCandidate struct {
	Candidate        Candidate
	Disqualified     bool
	DisqualReasons   []string
	PreferredScore   float64
	PreferredVector  [7]float64 // res, source, codec, hdr, audio, group, sizeEff
	InEquivBand      bool
	TiebreakScore    float64
	TiebreakVector   []float64
	Viable           bool
	KeptRank         int // 1 = primary winner, 2..K = runner-up, 0 = not kept
	KeptByHysteresis bool
	PruneReason      string // "", "dominated", "disqualified", "subsumed_by_pack", "subsumed_by_episodes"
}

// SlotDecision is the deterministic output for a single movie or TV slot.
type SlotDecision struct {
	SlotKey          string
	Kind             SlotKind
	Season           int
	Winners          []ScoredCandidate // rank 1..1+KeepRunnerUp
	Losers           []ScoredCandidate // eligible but dominated (prune candidates)
	Disqualified     []ScoredCandidate // failed Tier 1 required gate
	NeedsSlotRevival bool              // true if prior incumbent died and 0 viable winners remain
	HysteresisHeld   bool
}

// TitleDecision aggregates all slot decisions for a single movie or TV show.
type TitleDecision struct {
	ContentType   string
	ContentSource string
	ContentID     string
	Slots         []SlotDecision
}

// EvaluateSlot runs the three-tier rule engine on one slot with strict determinism.
func EvaluateSlot(
	cfg ProfileConfig,
	slotKey string,
	kind SlotKind,
	season int,
	candidates []Candidate,
	incumbent *IncumbentState,
	now time.Time,
) SlotDecision {
	dec := SlotDecision{
		SlotKey: slotKey,
		Kind:    kind,
		Season:  season,
	}
	if len(candidates) == 0 {
		if incumbent != nil {
			dec.NeedsSlotRevival = true
		}
		return dec
	}

	// Tier 1: Hard Gate (required)
	var survivors []ScoredCandidate
	for _, c := range candidates {
		reasons := evaluateRequired(cfg, kind, c, now)
		sc := ScoredCandidate{
			Candidate:      c,
			Disqualified:   len(reasons) > 0,
			DisqualReasons: reasons,
			Viable:         isViable(cfg, c, now),
		}
		if sc.Disqualified {
			sc.PruneReason = "disqualified:" + reasons[0]
			dec.Disqualified = append(dec.Disqualified, sc)
			continue
		}
		// Tier 2: Ordered Ranking (preferred)
		sc.PreferredScore, sc.PreferredVector = evaluatePreferred(cfg, kind, c)
		survivors = append(survivors, sc)
	}

	// Sort disqualified deterministically by info_hash for stable reports/tests.
	slices.SortFunc(dec.Disqualified, func(a, b ScoredCandidate) int {
		return bytes.Compare(a.Candidate.InfoHash, b.Candidate.InfoHash)
	})

	if len(survivors) == 0 {
		if incumbent != nil {
			dec.NeedsSlotRevival = true
		}
		return dec
	}

	// Identify top preferred score and equivalence band.
	topPref := survivors[0].PreferredScore
	topVec := survivors[0].PreferredVector
	for i := 1; i < len(survivors); i++ {
		if cfg.Preferred.RankingMode == RankingModeLexicographic {
			if compareFloatVecDesc(survivors[i].PreferredVector[:], topVec[:]) < 0 {
				topVec = survivors[i].PreferredVector
				topPref = survivors[i].PreferredScore
			}
		} else if survivors[i].PreferredScore > topPref {
			topPref = survivors[i].PreferredScore
		}
	}

	// Tier 3: Crowd Refinement / Tie-Breaker (if_found)
	for i := range survivors {
		if cfg.Preferred.RankingMode == RankingModeLexicographic {
			survivors[i].InEquivBand = equalFloatVec(survivors[i].PreferredVector[:], topVec[:])
		} else {
			survivors[i].InEquivBand = (topPref - survivors[i].PreferredScore) <= cfg.Preferred.EquivalenceBand+1e-9
		}
		survivors[i].TiebreakScore, survivors[i].TiebreakVector = evaluateIfFound(cfg, survivors[i].Candidate)
	}

	// Deterministic total-order ranking across all survivors:
	//  1. Viable candidates always outrank dead/unviable ones.
	//  2. Equivalence-band candidates outrank below-band candidates; inside the
	//     band (weighted mode), Tier 3 if_found refines the winner first, then
	//     preferred score breaks residual ties.
	//  3. Outside the band (or lexicographic mode), preferred order leads, then if_found.
	//  4. Mandatory final anchor: info_hash ASC (guarantees strict total order).
	slices.SortFunc(survivors, func(a, b ScoredCandidate) int {
		if a.Viable != b.Viable {
			if a.Viable {
				return -1
			}
			return 1
		}
		if cfg.Preferred.RankingMode == RankingModeLexicographic {
			if cmp := compareFloatVecDesc(a.PreferredVector[:], b.PreferredVector[:]); cmp != 0 {
				return cmp
			}
			if cmp := compareFloatVecDesc(a.TiebreakVector, b.TiebreakVector); cmp != 0 {
				return cmp
			}
			return bytes.Compare(a.Candidate.InfoHash, b.Candidate.InfoHash)
		}
		if a.InEquivBand != b.InEquivBand {
			if a.InEquivBand {
				return -1
			}
			return 1
		}
		if a.InEquivBand && b.InEquivBand {
			// Both in top quality equivalence band: Tier 3 if_found refines!
			if cmp := compareFloatVecDesc(a.TiebreakVector, b.TiebreakVector); cmp != 0 {
				return cmp
			}
			if math.Abs(a.PreferredScore-b.PreferredScore) > 1e-9 {
				if a.PreferredScore > b.PreferredScore {
					return -1
				}
				return 1
			}
			return bytes.Compare(a.Candidate.InfoHash, b.Candidate.InfoHash)
		}
		if math.Abs(a.PreferredScore-b.PreferredScore) > 1e-9 {
			if a.PreferredScore > b.PreferredScore {
				return -1
			}
			return 1
		}
		if cmp := compareFloatVecDesc(a.TiebreakVector, b.TiebreakVector); cmp != 0 {
			return cmp
		}
		return bytes.Compare(a.Candidate.InfoHash, b.Candidate.InfoHash)
	})

	// Apply Incumbent Hysteresis to prevent flip-flop churn on marginal swings.
	if incumbent != nil && len(survivors) > 1 {
		incIdx := -1
		for i := range survivors {
			if bytes.Equal(survivors[i].Candidate.InfoHash, incumbent.InfoHash) {
				incIdx = i
				break
			}
		}
		if incIdx > 0 && survivors[incIdx].Viable {
			challenger := survivors[0]
			inc := survivors[incIdx]
			if shouldHoldIncumbent(cfg, inc, challenger, incumbent.IncumbentSince, now) {
				// Promote viable incumbent back to index 0.
				inc.KeptByHysteresis = true
				copy(survivors[1:incIdx+1], survivors[0:incIdx])
				survivors[0] = inc
				dec.HysteresisHeld = true
			}
		}
	}

	if !survivors[0].Viable {
		dec.NeedsSlotRevival = true
	}

	keepCount := 1 + cfg.Resilience.KeepRunnerUp
	if keepCount > len(survivors) {
		keepCount = len(survivors)
	}
	for i := 0; i < keepCount; i++ {
		survivors[i].KeptRank = i + 1
		survivors[i].PruneReason = ""
		dec.Winners = append(dec.Winners, survivors[i])
	}
	for i := keepCount; i < len(survivors); i++ {
		survivors[i].KeptRank = 0
		survivors[i].PruneReason = "dominated"
		dec.Losers = append(dec.Losers, survivors[i])
	}
	return dec
}

// ResolveTitle groups all candidates of one movie or TV show into canonical slots,
// evaluates every slot, and applies cross-slot season_pack_policy resolution.
func ResolveTitle(
	cfg ProfileConfig,
	contentType, contentSource, contentID string,
	candidates []Candidate,
	incumbents map[string]IncumbentState,
	now time.Time,
) TitleDecision {
	td := TitleDecision{
		ContentType:   contentType,
		ContentSource: contentSource,
		ContentID:     contentID,
	}

	type slotBucket struct {
		assign     SlotAssignment
		candidates []Candidate
	}
	buckets := make(map[string]*slotBucket)
	for _, c := range candidates {
		assign, ok := DeriveSlot(c, cfg.Scope)
		if !ok {
			continue
		}
		b, exists := buckets[assign.SlotKey]
		if !exists {
			b = &slotBucket{assign: assign}
			buckets[assign.SlotKey] = b
		}
		b.candidates = append(b.candidates, c)
	}
	// Ensure existing incumbent slots with 0 remaining candidates are checked for revival.
	for slotKey := range incumbents {
		if _, exists := buckets[slotKey]; !exists {
			s, _ := ParseSeasonFromSlotKey(slotKey)
			kind := SlotKindMovie
			if contentType == "tv_show" {
				kind = SlotKindEpisode
			}
			buckets[slotKey] = &slotBucket{
				assign: SlotAssignment{SlotKey: slotKey, Kind: kind, Season: s},
			}
		}
	}

	slotKeys := make([]string, 0, len(buckets))
	for k := range buckets {
		slotKeys = append(slotKeys, k)
	}
	slices.Sort(slotKeys)

	decisions := make([]SlotDecision, 0, len(slotKeys))
	for _, key := range slotKeys {
		b := buckets[key]
		var inc *IncumbentState
		if existing, ok := incumbents[key]; ok {
			cp := existing
			inc = &cp
		}
		decisions = append(decisions, EvaluateSlot(cfg, key, b.assign.Kind, b.assign.Season, b.candidates, inc, now))
	}

	// Cross-slot TV season_pack_policy resolution.
	if contentType == "tv_show" {
		switch cfg.Scope.SeasonPackPolicy {
		case SeasonPackPreferPack:
			// Find seasons with a viable winning season pack.
			coveredSeasons := make(map[int]bool)
			for _, d := range decisions {
				if d.Kind == SlotKindSeasonPack && len(d.Winners) > 0 && d.Winners[0].Viable {
					coveredSeasons[d.Season] = true
				}
			}
			for i := range decisions {
				d := &decisions[i]
				if (d.Kind == SlotKindEpisode || d.Kind == SlotKindMultiEpisode) && coveredSeasons[d.Season] {
					for _, w := range d.Winners {
						w.KeptRank = 0
						w.PruneReason = "subsumed_by_pack"
						d.Losers = append(d.Losers, w)
					}
					d.Winners = nil
					d.NeedsSlotRevival = false
				}
			}
		case SeasonPackPreferIndividual:
			// Seasons that already have individual episode winners demote redundant season packs.
			seasonsWithIndividual := make(map[int]int)
			for _, d := range decisions {
				if d.Kind == SlotKindEpisode && len(d.Winners) > 0 && d.Winners[0].Viable {
					seasonsWithIndividual[d.Season]++
				}
			}
			for i := range decisions {
				d := &decisions[i]
				if d.Kind == SlotKindSeasonPack && seasonsWithIndividual[d.Season] > 0 {
					for _, w := range d.Winners {
						w.KeptRank = 0
						w.PruneReason = "subsumed_by_episodes"
						d.Losers = append(d.Losers, w)
					}
					d.Winners = nil
					d.NeedsSlotRevival = false
				}
			}
		}
	}

	td.Slots = decisions
	return td
}

func evaluateRequired(cfg ProfileConfig, kind SlotKind, c Candidate, now time.Time) []string {
	r := cfg.Required
	var reasons []string

	if r.RequireFilesInfo && c.FilesStatus != "" && c.FilesStatus != model.FilesStatusSingle.String() && c.FilesStatus != model.FilesStatusMulti.String() {
		reasons = append(reasons, "files_info_missing")
	}
	if (r.RequireArmigerApproved || r.RequirePublicationReview != "") && !c.ArmigerApproved {
		reasons = append(reasons, "publication_review_missing")
	}
	if r.RequirePayloadProof && !c.PayloadProofActive {
		reasons = append(reasons, "payload_proof_missing")
	}
	if r.Exclude3D && c.Video3D != "" {
		reasons = append(reasons, "video_3d_excluded")
	}

	// Resolution checks
	normRes := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(c.VideoResolution), "V"))
	resRank := resolutionRankMap[normRes]
	if r.MinResolution != "" {
		if resRank == 0 || resRank < resolutionRankMap[r.MinResolution] {
			reasons = append(reasons, "resolution_below_min")
		}
	}
	if r.MaxResolution != "" {
		if resRank == 0 || resRank > resolutionRankMap[r.MaxResolution] {
			reasons = append(reasons, "resolution_above_max")
		}
	}
	if len(r.ResolutionsAllowed) > 0 {
		if normRes == "" || !slices.Contains(r.ResolutionsAllowed, normRes) {
			reasons = append(reasons, "resolution_not_allowed")
		}
	}

	// Source / Modifier checks
	effSource := effectiveSourceToken(c)
	if r.ExcludeCamTS {
		if effSource == "CAM" || effSource == "TELESYNC" || effSource == "TELECINE" || effSource == "WORKPRINT" || strings.EqualFold(c.VideoModifier, "SCREENER") {
			reasons = append(reasons, "cam_ts_excluded")
		}
	}
	if len(r.SourcesBlocked) > 0 && effSource != "" && slices.Contains(r.SourcesBlocked, effSource) {
		reasons = append(reasons, "source_blocked")
	}
	if len(r.SourcesAllowed) > 0 {
		if effSource == "" || !slices.Contains(r.SourcesAllowed, effSource) {
			reasons = append(reasons, "source_not_allowed")
		}
	}
	normMod := strings.ToUpper(strings.TrimSpace(c.VideoModifier))
	if len(r.ModifiersBlocked) > 0 && normMod != "" && slices.Contains(r.ModifiersBlocked, normMod) {
		reasons = append(reasons, "modifier_blocked")
	}
	if len(r.ModifiersAllowed) > 0 {
		if normMod == "" || !slices.Contains(r.ModifiersAllowed, normMod) {
			reasons = append(reasons, "modifier_not_allowed")
		}
	}

	// Codec checks (family-aware)
	normCodec := normalizeCodecToken(c.VideoCodec)
	if len(r.CodecsBlocked) > 0 && normCodec != "" && codecMatchesAny(normCodec, r.CodecsBlocked) {
		reasons = append(reasons, "codec_blocked")
	}
	if len(r.CodecsAllowed) > 0 {
		if normCodec == "" || !codecMatchesAny(normCodec, r.CodecsAllowed) {
			reasons = append(reasons, "codec_not_allowed")
		}
	}

	// Language checks
	if r.RequireEnglishAudio {
		hasEn := (c.EnglishAudio != nil && *c.EnglishAudio) || containsFold(c.Languages, "en") || containsFold(c.Languages, "eng") || containsFold(c.Languages, "english")
		if !hasEn {
			reasons = append(reasons, "english_audio_missing")
		}
	}
	if len(r.LanguagesRequired) > 0 {
		matchedLang := false
		for _, want := range r.LanguagesRequired {
			if containsFold(c.Languages, want) {
				matchedLang = true
				break
			}
			if want == "en" && c.EnglishAudio != nil && *c.EnglishAudio {
				matchedLang = true
				break
			}
		}
		if !matchedLang {
			reasons = append(reasons, "language_missing")
		}
	}

	// HDR checks
	if len(r.HDRRequired) > 0 {
		matchedHDR := false
		for _, want := range r.HDRRequired {
			if want == "SDR" && len(c.HDRFormats) == 0 {
				matchedHDR = true
				break
			}
			if containsFold(c.HDRFormats, want) {
				matchedHDR = true
				break
			}
		}
		if !matchedHDR {
			reasons = append(reasons, "hdr_missing")
		}
	}
	if len(r.HDRBlocked) > 0 {
		for _, bad := range r.HDRBlocked {
			if (bad == "SDR" && len(c.HDRFormats) == 0) || containsFold(c.HDRFormats, bad) {
				reasons = append(reasons, "hdr_blocked")
				break
			}
		}
	}

	// Audio checks
	if len(r.AudioFormatsAllowed) > 0 {
		matched := false
		for _, f := range c.AudioFormats {
			if slices.Contains(r.AudioFormatsAllowed, normalizeAudioToken(f)) {
				matched = true
				break
			}
		}
		if !matched {
			reasons = append(reasons, "audio_format_not_allowed")
		}
	}
	if len(r.AudioFeaturesRequired) > 0 {
		matched := false
		for _, want := range r.AudioFeaturesRequired {
			if containsFold(c.AudioFeatures, want) || containsFold(c.AudioFormats, want) {
				matched = true
				break
			}
		}
		if !matched {
			reasons = append(reasons, "audio_feature_missing")
		}
	}

	// Seeders check (honors honest-unknown grace when RequireKnownSeeders=false)
	if r.MinSeeders > 0 {
		if c.Seeders == nil {
			if cfg.Resilience.RequireKnownSeeders || now.Sub(c.CreatedAt) > cfg.Resilience.UnknownSeedersGrace.Std() {
				reasons = append(reasons, "seeders_unknown")
			}
		} else if *c.Seeders < r.MinSeeders {
			reasons = append(reasons, "seeders_below_min")
		}
	}

	// Size limits (slot-kind specific override -> generic fallback)
	sizeMB := float64(c.SizeBytes) / (1024 * 1024)
	minMB, maxMB := effectiveSizeLimits(r, kind)
	if minMB > 0 && sizeMB < minMB {
		reasons = append(reasons, "size_below_min")
	}
	if maxMB > 0 && sizeMB > maxMB {
		reasons = append(reasons, "size_above_max")
	}

	// Release group checks
	group := strings.ToLower(strings.TrimSpace(c.ReleaseGroup))
	if len(r.ReleaseGroupsBlocked) > 0 && group != "" && slices.Contains(r.ReleaseGroupsBlocked, group) {
		reasons = append(reasons, "release_group_blocked")
	}
	if len(r.ReleaseGroupsAllowed) > 0 {
		if group == "" || !slices.Contains(r.ReleaseGroupsAllowed, group) {
			reasons = append(reasons, "release_group_not_allowed")
		}
	}

	// Title exclusion regexes
	for _, re := range cfg.compiledExcludePatterns {
		if re.MatchString(c.Name) {
			reasons = append(reasons, "title_pattern_excluded")
			break
		}
	}

	return reasons
}

func isViable(cfg ProfileConfig, c Candidate, now time.Time) bool {
	if c.LivenessDead {
		return false
	}
	if cfg.Required.RequirePayloadProof && !c.PayloadProofActive {
		return false
	}
	minViable := cfg.Resilience.MinViableSeeders
	if minViable <= 0 {
		return true
	}
	if c.Seeders == nil {
		if cfg.Resilience.RequireKnownSeeders {
			return false
		}
		return now.Sub(c.CreatedAt) <= cfg.Resilience.UnknownSeedersGrace.Std()
	}
	if *c.Seeders == 0 && !c.SeedersAuthoritative && now.Sub(c.CreatedAt) <= cfg.Resilience.UnknownSeedersGrace.Std() {
		return true
	}
	return *c.Seeders >= minViable
}

func evaluatePreferred(cfg ProfileConfig, kind SlotKind, c Candidate) (float64, [7]float64) {
	p := cfg.Preferred
	w := p.Weights
	var vec [7]float64

	// 0: Resolution
	normRes := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(c.VideoResolution), "V"))
	if len(p.ResolutionOrder) == 1 && p.ResolutionOrder[0] == "highest" {
		if rk := resolutionRankMap[normRes]; rk > 0 {
			vec[0] = (float64(rk) / 9.0) * w.Resolution
		}
	} else if len(p.ResolutionOrder) > 0 {
		vec[0] = rankScore(normRes, p.ResolutionOrder, w.Resolution, exactMatch)
	}

	// 1: Source (REMUX subsumes BluRay when present)
	if len(p.SourceOrder) > 0 {
		vec[1] = rankScore(effectiveSourceToken(c), p.SourceOrder, w.Source, exactMatch)
	}

	// 2: Codec (family-aware fallback)
	if len(p.CodecOrder) > 0 {
		normCodec := normalizeCodecToken(c.VideoCodec)
		vec[2] = rankScore(normCodec, p.CodecOrder, w.Codec, codecMatchesToken)
	}

	// 3: HDR
	if len(p.HDROrder) > 0 {
		bestHDR := 0.0
		if len(c.HDRFormats) == 0 {
			bestHDR = rankScore("SDR", p.HDROrder, w.HDR, exactMatch)
		} else {
			for _, h := range c.HDRFormats {
				if s := rankScore(normalizeHDRToken(h), p.HDROrder, w.HDR, exactMatch); s > bestHDR {
					bestHDR = s
				}
			}
		}
		vec[3] = bestHDR
	}

	// 4: Audio
	if len(p.AudioOrder) > 0 {
		bestAudio := 0.0
		for _, f := range c.AudioFeatures {
			if s := rankScore(normalizeAudioToken(f), p.AudioOrder, w.Audio, exactMatch); s > bestAudio {
				bestAudio = s
			}
		}
		for _, f := range c.AudioFormats {
			if s := rankScore(normalizeAudioToken(f), p.AudioOrder, w.Audio, exactMatch); s > bestAudio {
				bestAudio = s
			}
		}
		vec[4] = bestAudio
	}

	// 5: Preferred release group
	if len(p.PreferredGroups) > 0 {
		g := strings.ToLower(strings.TrimSpace(c.ReleaseGroup))
		vec[5] = rankScore(g, p.PreferredGroups, w.Group, exactMatch)
	}

	// 6: Size efficiency
	vec[6] = evaluateSizeEfficiency(p.SizeEfficiency, kind, c.SizeBytes, w.SizeEfficiency)

	total := vec[0] + vec[1] + vec[2] + vec[3] + vec[4] + vec[5] + vec[6]
	return total, vec
}

func evaluateSizeEfficiency(cfg SizeEfficiencyConfig, kind SlotKind, sizeBytes uint64, weight float64) float64 {
	if cfg.Mode == SizeEffNone || weight <= 0 || sizeBytes == 0 {
		return 0
	}
	sizeMB := float64(sizeBytes) / (1024 * 1024)
	switch cfg.Mode {
	case SizeEffPreferSmaller:
		// Smooth monotonic decay favoring compact releases without collapsing differences.
		return weight / (1.0 + math.Log1p(sizeMB/512.0))
	case SizeEffPreferLarger:
		return weight * (1.0 - 1.0/(1.0+math.Log1p(sizeMB/2048.0)))
	case SizeEffTargetWindow:
		var win [2]float64
		switch kind {
		case SlotKindMovie:
			win = cfg.TargetMBMovie
		case SlotKindSeasonPack, SlotKindSeriesPack:
			win = cfg.TargetMBSeasonPack
		default:
			win = cfg.TargetMBEpisode
		}
		if win[1] <= 0 || win[1] < win[0] {
			return 0
		}
		if sizeMB >= win[0] && sizeMB <= win[1] {
			return weight
		}
		dist := 0.0
		if sizeMB < win[0] {
			dist = (win[0] - sizeMB) / math.Max(win[0], 1.0)
		} else {
			dist = (sizeMB - win[1]) / math.Max(win[1], 1.0)
		}
		return weight * math.Max(0, 1.0-dist)
	default:
		return 0
	}
}

func evaluateIfFound(cfg ProfileConfig, c Candidate) (float64, []float64) {
	vec := make([]float64, len(cfg.IfFound.Order))
	combined := 0.0
	weight := 1000.0
	for i, rule := range cfg.IfFound.Order {
		var v float64
		switch rule {
		case TieBreakRepackProper:
			if hasRepackOrProper(c.Revisions) {
				v = 1.0
			}
		case TieBreakPreferredGroup:
			g := strings.ToLower(strings.TrimSpace(c.ReleaseGroup))
			groups := cfg.IfFound.PreferredGroups
			if len(groups) == 0 {
				groups = cfg.Preferred.PreferredGroups
			}
			v = rankScore(g, groups, 1.0, exactMatch)
		case TieBreakHighestSeeders:
			if c.Seeders != nil && *c.Seeders > 0 {
				raw := float64(*c.Seeders)
				if cfg.IfFound.SeederBucketFactor > 1.0 {
					v = math.Floor(math.Log(raw)/math.Log(cfg.IfFound.SeederBucketFactor) + 1.0)
				} else {
					v = math.Log1p(raw)
				}
			}
		case TieBreakCleanSingleMediaFile:
			if !c.HasClutterFiles && (c.FilesStatus == model.FilesStatusSingle.String() || c.VideoFileCount == 1 || c.FilesCount == 1) {
				v = 1.0
			}
		case TieBreakEmbeddedSubtitles:
			if c.HasEmbeddedSubtitles {
				v = 1.0
			}
		case TieBreakNewestVerified:
			var ts time.Time
			switch {
			case c.PayloadProofVerifiedAt != nil:
				ts = *c.PayloadProofVerifiedAt
			case c.ArmigerEvaluatedAt != nil:
				ts = *c.ArmigerEvaluatedAt
			default:
				ts = c.PublishedAt
			}
			if !ts.IsZero() {
				v = float64(ts.Unix()) / 1e10
			}
		case TieBreakSmallestSizeInBand:
			if c.SizeBytes > 0 {
				v = -float64(c.SizeBytes)
			}
		case TieBreakLargestSizeInBand:
			v = float64(c.SizeBytes)
		}
		vec[i] = v
		combined += v * weight
		weight *= 0.1
	}
	return combined, vec
}

func shouldHoldIncumbent(cfg ProfileConfig, inc, challenger ScoredCandidate, incumbentSince, now time.Time) bool {
	h := cfg.Resilience.Hysteresis
	if h.MinPreferredDelta <= 0 && h.MinSeederRatio <= 1.0 && h.IncumbentGrace.Std() <= 0 {
		return false
	}
	// REPACK/PROPER override: a proper/repack in the same quality tier immediately dethrones non-repack.
	if h.AllowRepackOverride && hasRepackOrProper(challenger.Candidate.Revisions) && !hasRepackOrProper(inc.Candidate.Revisions) {
		if challenger.PreferredScore >= inc.PreferredScore-cfg.Preferred.EquivalenceBand {
			return false
		}
	}
	prefDelta := challenger.PreferredScore - inc.PreferredScore
	if prefDelta >= h.MinPreferredDelta && h.MinPreferredDelta > 0 {
		return false
	}
	// If challenger is only winning on preferred by less than MinPreferredDelta (or only on tiebreak):
	if h.IncumbentGrace.Std() > 0 && !incumbentSince.IsZero() && now.Sub(incumbentSince) < h.IncumbentGrace.Std() {
		return true
	}
	// Check seeder ratio when both are in equivalence band.
	if inc.InEquivBand && challenger.InEquivBand && h.MinSeederRatio > 1.0 {
		incSeeds := 0
		if inc.Candidate.Seeders != nil {
			incSeeds = *inc.Candidate.Seeders
		}
		chalSeeds := 0
		if challenger.Candidate.Seeders != nil {
			chalSeeds = *challenger.Candidate.Seeders
		}
		if incSeeds > 0 && float64(chalSeeds) < float64(incSeeds)*h.MinSeederRatio && prefDelta < h.MinPreferredDelta {
			return true
		}
	}
	return prefDelta < h.MinPreferredDelta && h.MinPreferredDelta > 0 && !challenger.InEquivBand
}

func effectiveSourceToken(c Candidate) string {
	if strings.EqualFold(strings.TrimSpace(c.VideoModifier), "REMUX") {
		return "REMUX"
	}
	return normalizeSourceToken(c.VideoSource)
}

func effectiveSizeLimits(r RequiredConfig, kind SlotKind) (float64, float64) {
	minMB, maxMB := r.MinSizeMB, r.MaxSizeMB
	switch kind {
	case SlotKindMovie:
		if r.MinSizeMBMovie > 0 {
			minMB = r.MinSizeMBMovie
		}
		if r.MaxSizeMBMovie > 0 {
			maxMB = r.MaxSizeMBMovie
		}
	case SlotKindEpisode, SlotKindMultiEpisode:
		if r.MinSizeMBEpisode > 0 {
			minMB = r.MinSizeMBEpisode
		}
		if r.MaxSizeMBEpisode > 0 {
			maxMB = r.MaxSizeMBEpisode
		}
	case SlotKindSeasonPack, SlotKindSeriesPack:
		if r.MinSizeMBSeasonPack > 0 {
			minMB = r.MinSizeMBSeasonPack
		}
		if r.MaxSizeMBSeasonPack > 0 {
			maxMB = r.MaxSizeMBSeasonPack
		}
	}
	return minMB, maxMB
}

func codecFamilyKey(norm string) string {
	switch norm {
	case "H264", "X264":
		return "H264"
	case "HEVC", "X265", "H265":
		return "HEVC"
	case "MPEG4", "XVID", "DIVX":
		return "MPEG4"
	default:
		return norm
	}
}

func codecMatchesToken(cand, target string) bool {
	if cand == "" || target == "" {
		return false
	}
	if cand == target {
		return true
	}
	// Broad family tokens ("H264", "HEVC") match specific encoders ("X264", "X265");
	// specific encoder tokens ("X265") match only exact OR generic family if preferred.
	if target == "H264" && cand == "X264" {
		return true
	}
	if target == "HEVC" && cand == "X265" {
		return true
	}
	return codecFamilyKey(cand) == codecFamilyKey(target) && (target == "H264" || target == "HEVC")
}

func codecMatchesAny(cand string, allowed []string) bool {
	for _, tok := range allowed {
		if cand == tok || codecMatchesToken(cand, tok) {
			return true
		}
	}
	return false
}

func exactMatch(a, b string) bool {
	return a != "" && a == b
}

func rankScore(val string, order []string, weight float64, matchFn func(string, string) bool) float64 {
	if val == "" || len(order) == 0 || weight <= 0 {
		return 0
	}
	n := len(order)
	// First check exact match so x265 ranks explicitly over generic HEVC when both appear.
	for i, tok := range order {
		if val == tok {
			return weight * float64(n-i) / float64(n)
		}
	}
	for i, tok := range order {
		if matchFn(val, tok) {
			return weight * float64(n-i) / float64(n)
		}
	}
	return 0
}

func containsFold(slice []string, target string) bool {
	for _, s := range slice {
		if strings.EqualFold(strings.TrimSpace(s), target) {
			return true
		}
	}
	return false
}

func hasRepackOrProper(revs []string) bool {
	for _, r := range revs {
		if strings.EqualFold(r, "REPACK") || strings.EqualFold(r, "PROPER") {
			return true
		}
	}
	return false
}

func compareFloatVecDesc(a, b []float64) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if math.Abs(a[i]-b[i]) > 1e-9 {
			if a[i] > b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func equalFloatVec(a, b []float64) bool {
	return compareFloatVecDesc(a, b) == 0
}

// ExplainRow is a concise JSON-serializable explanation of one candidate's evaluation.
type ExplainRow struct {
	InfoHash         string   `json:"info_hash"`
	Name             string   `json:"name"`
	Resolution       string   `json:"resolution,omitempty"`
	Source           string   `json:"source,omitempty"`
	Codec            string   `json:"codec,omitempty"`
	ReleaseGroup     string   `json:"release_group,omitempty"`
	SizeMB           float64  `json:"size_mb"`
	Seeders          *int     `json:"seeders,omitempty"`
	KeptRank         int      `json:"kept_rank"`
	Viable           bool     `json:"viable"`
	InEquivBand      bool     `json:"in_equiv_band"`
	PreferredScore   float64  `json:"preferred_score"`
	TiebreakScore    float64  `json:"tiebreak_score"`
	Disqualified     bool     `json:"disqualified"`
	DisqualReasons   []string `json:"disqual_reasons,omitempty"`
	PruneReason      string   `json:"prune_reason,omitempty"`
	KeptByHysteresis bool     `json:"kept_by_hysteresis,omitempty"`
}

// ExplainDecision converts a SlotDecision into an inspectable explanation list.
func ExplainDecision(d SlotDecision) []ExplainRow {
	all := make([]ScoredCandidate, 0, len(d.Winners)+len(d.Losers)+len(d.Disqualified))
	all = append(all, d.Winners...)
	all = append(all, d.Losers...)
	all = append(all, d.Disqualified...)
	out := make([]ExplainRow, 0, len(all))
	for _, sc := range all {
		out = append(out, ExplainRow{
			InfoHash:         hex.EncodeToString(sc.Candidate.InfoHash),
			Name:             sc.Candidate.Name,
			Resolution:       sc.Candidate.VideoResolution,
			Source:           effectiveSourceToken(sc.Candidate),
			Codec:            sc.Candidate.VideoCodec,
			ReleaseGroup:     sc.Candidate.ReleaseGroup,
			SizeMB:           math.Round((float64(sc.Candidate.SizeBytes)/(1024*1024))*10) / 10,
			Seeders:          sc.Candidate.Seeders,
			KeptRank:         sc.KeptRank,
			Viable:           sc.Viable,
			InEquivBand:      sc.InEquivBand,
			PreferredScore:   math.Round(sc.PreferredScore*100) / 100,
			TiebreakScore:    math.Round(sc.TiebreakScore*100) / 100,
			Disqualified:     sc.Disqualified,
			DisqualReasons:   sc.DisqualReasons,
			PruneReason:      sc.PruneReason,
			KeptByHysteresis: sc.KeptByHysteresis,
		})
	}
	return out
}

func FormatReasonSummary(d SlotDecision) string {
	return fmt.Sprintf("slot=%s winners=%d losers=%d disqualified=%d revival=%v",
		d.SlotKey, len(d.Winners), len(d.Losers), len(d.Disqualified), d.NeedsSlotRevival)
}
