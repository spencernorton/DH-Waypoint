package leanmode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/model"
	"gopkg.in/yaml.v3"
)

const (
	SchemaVersion = 1

	ModeObserve = "observe"
	ModeServe   = "serve"
	ModePrune   = "prune"

	SeasonPackSeparateSlot     = "separate_slot"
	SeasonPackPreferPack       = "prefer_pack"
	SeasonPackPreferIndividual = "prefer_individual"

	MultiEpisodeSeparateSlot     = "separate_slot"
	MultiEpisodePreferIndividual = "prefer_individual"

	UnlinkedPassThrough      = "pass_through"
	UnlinkedExcludeFromServe = "exclude_from_serve"
	UnlinkedPruneAfterAge    = "prune_after_age"

	NonVideoPassThrough      = "pass_through"
	NonVideoExcludeFromServe = "exclude_from_serve"

	RankingModeWeighted      = "weighted"
	RankingModeLexicographic = "lexicographic"

	SizeEffNone          = "none"
	SizeEffTargetWindow  = "target_window"
	SizeEffPreferSmaller = "prefer_smaller"
	SizeEffPreferLarger  = "prefer_larger"

	TieBreakRepackProper         = "repack_proper"
	TieBreakPreferredGroup       = "preferred_group"
	TieBreakHighestSeeders       = "highest_seeders"
	TieBreakCleanSingleMediaFile = "clean_single_media_file"
	TieBreakEmbeddedSubtitles    = "embedded_subtitles"
	TieBreakNewestVerified       = "newest_verified"
	TieBreakSmallestSizeInBand   = "smallest_size_in_band"
	TieBreakLargestSizeInBand    = "largest_size_in_band"
)

var (
	ProfileIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

	resolutionRankMap = map[string]int{
		"360p":  1,
		"480p":  2,
		"540p":  3,
		"576p":  4,
		"720p":  5,
		"1080p": 6,
		"1440p": 7,
		"2160p": 8,
		"4320p": 9,
	}

	validSources = map[string]bool{
		"CAM":       true,
		"TELESYNC":  true,
		"TELECINE":  true,
		"WORKPRINT": true,
		"DVD":       true,
		"TV":        true,
		"WEBDL":     true,
		"WEBRIP":    true,
		"BLURAY":    true,
		"REMUX":     true,
	}

	validCodecs = map[string]bool{
		"H264":  true,
		"X264":  true,
		"H265":  true,
		"HEVC":  true,
		"X265":  true,
		"AV1":   true,
		"XVID":  true,
		"DIVX":  true,
		"MPEG2": true,
		"MPEG4": true,
	}

	validModifiers = map[string]bool{
		"REGIONAL": true,
		"SCREENER": true,
		"RAWHD":    true,
		"BRDISK":   true,
		"REMUX":    true,
	}

	validHDR = map[string]bool{
		"DOLBY_VISION": true,
		"HDR10_PLUS":   true,
		"HDR10":        true,
		"HDR":          true,
		"SDR":          true,
	}

	validAudio = map[string]bool{
		"ATMOS":  true,
		"TRUEHD": true,
		"DTS_X":  true,
		"DTS":    true,
		"EAC3":   true,
		"FLAC":   true,
		"AC3":    true,
		"AAC":    true,
		"OPUS":   true,
	}

	validTieBreakers = map[string]bool{
		TieBreakRepackProper:         true,
		TieBreakPreferredGroup:       true,
		TieBreakHighestSeeders:       true,
		TieBreakCleanSingleMediaFile: true,
		TieBreakEmbeddedSubtitles:    true,
		TieBreakNewestVerified:       true,
		TieBreakSmallestSizeInBand:   true,
		TieBreakLargestSizeInBand:    true,
	}
)

// Duration wraps time.Duration for clean YAML string decoding (e.g. "72h", "24h").
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var raw string
	if err := value.Decode(&raw); err != nil {
		return err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "0" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d Duration) Std() time.Duration {
	return time.Duration(d)
}

// ProfileConfig is the declarative YAML specification for a LEAN MODE curation profile.
type ProfileConfig struct {
	Version     int              `yaml:"version" json:"version"`
	ID          string           `yaml:"id" json:"id"`
	Description string           `yaml:"description,omitempty" json:"description,omitempty"`
	Enabled     bool             `yaml:"enabled" json:"enabled"`
	Mode        string           `yaml:"mode" json:"mode"`
	Scope       ScopeConfig      `yaml:"scope" json:"scope"`
	Resilience  ResilienceConfig `yaml:"resilience" json:"resilience"`
	Required    RequiredConfig   `yaml:"required" json:"required"`
	Preferred   PreferredConfig  `yaml:"preferred" json:"preferred"`
	IfFound     IfFoundConfig    `yaml:"if_found" json:"if_found"`
	Prune       PruneConfig      `yaml:"prune" json:"prune"`

	// compiledExcludePatterns is derived at Validate time, excluded from config digest JSON.
	compiledExcludePatterns []*regexp.Regexp `yaml:"-" json:"-"`
}

type ScopeConfig struct {
	ContentTypes        []string `yaml:"content_types" json:"content_types"`
	SeasonPackPolicy    string   `yaml:"season_pack_policy" json:"season_pack_policy"`
	MultiEpisodePolicy  string   `yaml:"multi_episode_policy" json:"multi_episode_policy"`
	UnlinkedPolicy      string   `yaml:"unlinked_policy" json:"unlinked_policy"`
	UnlinkedPruneMinAge Duration `yaml:"unlinked_prune_min_age,omitempty" json:"unlinked_prune_min_age,omitempty"`
	NonVideoPolicy      string   `yaml:"non_video_policy" json:"non_video_policy"`
}

type ResilienceConfig struct {
	KeepRunnerUp        int              `yaml:"keep_runner_up" json:"keep_runner_up"`
	MinViableSeeders    int              `yaml:"min_viable_seeders" json:"min_viable_seeders"`
	RequireKnownSeeders bool             `yaml:"require_known_seeders" json:"require_known_seeders"`
	UnknownSeedersGrace Duration         `yaml:"unknown_seeders_grace" json:"unknown_seeders_grace"`
	Hysteresis          HysteresisConfig `yaml:"hysteresis" json:"hysteresis"`
}

type HysteresisConfig struct {
	MinPreferredDelta   float64  `yaml:"min_preferred_delta" json:"min_preferred_delta"`
	MinSeederRatio      float64  `yaml:"min_seeder_ratio" json:"min_seeder_ratio"`
	AllowRepackOverride bool     `yaml:"allow_repack_override" json:"allow_repack_override"`
	IncumbentGrace      Duration `yaml:"incumbent_grace_period" json:"incumbent_grace_period"`
}

// RequiredConfig defines Tier 1 (Hard Gate) disqualification rules.
type RequiredConfig struct {
	MinResolution            string   `yaml:"min_resolution,omitempty" json:"min_resolution,omitempty"`
	MaxResolution            string   `yaml:"max_resolution,omitempty" json:"max_resolution,omitempty"`
	ResolutionsAllowed       []string `yaml:"resolutions_allowed,omitempty" json:"resolutions_allowed,omitempty"`
	SourcesAllowed           []string `yaml:"sources_allowed,omitempty" json:"sources_allowed,omitempty"`
	SourcesBlocked           []string `yaml:"sources_blocked,omitempty" json:"sources_blocked,omitempty"`
	ExcludeCamTS             bool     `yaml:"exclude_cam_ts" json:"exclude_cam_ts"`
	CodecsAllowed            []string `yaml:"codecs_allowed,omitempty" json:"codecs_allowed,omitempty"`
	CodecsBlocked            []string `yaml:"codecs_blocked,omitempty" json:"codecs_blocked,omitempty"`
	ModifiersAllowed         []string `yaml:"modifiers_allowed,omitempty" json:"modifiers_allowed,omitempty"`
	ModifiersBlocked         []string `yaml:"modifiers_blocked,omitempty" json:"modifiers_blocked,omitempty"`
	Exclude3D                bool     `yaml:"exclude_3d" json:"exclude_3d"`
	LanguagesRequired        []string `yaml:"languages_required,omitempty" json:"languages_required,omitempty"`
	RequireEnglishAudio      bool     `yaml:"require_english_audio" json:"require_english_audio"`
	HDRRequired              []string `yaml:"hdr_required,omitempty" json:"hdr_required,omitempty"`
	HDRBlocked               []string `yaml:"hdr_blocked,omitempty" json:"hdr_blocked,omitempty"`
	AudioFormatsAllowed      []string `yaml:"audio_formats_allowed,omitempty" json:"audio_formats_allowed,omitempty"`
	AudioFeaturesRequired    []string `yaml:"audio_features_required,omitempty" json:"audio_features_required,omitempty"`
	MinSeeders               int      `yaml:"min_seeders" json:"min_seeders"`
	MinSizeMB                float64  `yaml:"min_size_mb,omitempty" json:"min_size_mb,omitempty"`
	MaxSizeMB                float64  `yaml:"max_size_mb,omitempty" json:"max_size_mb,omitempty"`
	MinSizeMBMovie           float64  `yaml:"min_size_mb_movie,omitempty" json:"min_size_mb_movie,omitempty"`
	MaxSizeMBMovie           float64  `yaml:"max_size_mb_movie,omitempty" json:"max_size_mb_movie,omitempty"`
	MinSizeMBEpisode         float64  `yaml:"min_size_mb_episode,omitempty" json:"min_size_mb_episode,omitempty"`
	MaxSizeMBEpisode         float64  `yaml:"max_size_mb_episode,omitempty" json:"max_size_mb_episode,omitempty"`
	MinSizeMBSeasonPack      float64  `yaml:"min_size_mb_season_pack,omitempty" json:"min_size_mb_season_pack,omitempty"`
	MaxSizeMBSeasonPack      float64  `yaml:"max_size_mb_season_pack,omitempty" json:"max_size_mb_season_pack,omitempty"`
	ReleaseGroupsAllowed     []string `yaml:"release_groups_allowed,omitempty" json:"release_groups_allowed,omitempty"`
	ReleaseGroupsBlocked     []string `yaml:"release_groups_blocked,omitempty" json:"release_groups_blocked,omitempty"`
	RequireFilesInfo         bool     `yaml:"require_files_info" json:"require_files_info"`
	RequireArmigerApproved   bool     `yaml:"require_armiger_approved" json:"require_armiger_approved"`
	RequirePublicationReview string   `yaml:"require_publication_processor,omitempty" json:"require_publication_processor,omitempty"`
	RequirePayloadProof      bool     `yaml:"require_payload_proof" json:"require_payload_proof"`
	ExcludeTitlePatterns     []string `yaml:"exclude_title_patterns,omitempty" json:"exclude_title_patterns,omitempty"`
}

// PreferredConfig defines Tier 2 (Ordered Ranking) rules.
type PreferredConfig struct {
	RankingMode     string               `yaml:"ranking_mode" json:"ranking_mode"`
	EquivalenceBand float64              `yaml:"equivalence_band" json:"equivalence_band"`
	ResolutionOrder []string             `yaml:"resolution_order,omitempty" json:"resolution_order,omitempty"`
	SourceOrder     []string             `yaml:"source_order,omitempty" json:"source_order,omitempty"`
	CodecOrder      []string             `yaml:"codec_order,omitempty" json:"codec_order,omitempty"`
	HDROrder        []string             `yaml:"hdr_order,omitempty" json:"hdr_order,omitempty"`
	AudioOrder      []string             `yaml:"audio_order,omitempty" json:"audio_order,omitempty"`
	PreferredGroups []string             `yaml:"preferred_groups,omitempty" json:"preferred_groups,omitempty"`
	Weights         PreferredWeights     `yaml:"weights" json:"weights"`
	SizeEfficiency  SizeEfficiencyConfig `yaml:"size_efficiency" json:"size_efficiency"`
}

type PreferredWeights struct {
	Resolution     float64 `yaml:"resolution" json:"resolution"`
	Source         float64 `yaml:"source" json:"source"`
	Codec          float64 `yaml:"codec" json:"codec"`
	HDR            float64 `yaml:"hdr" json:"hdr"`
	Audio          float64 `yaml:"audio" json:"audio"`
	Group          float64 `yaml:"group" json:"group"`
	SizeEfficiency float64 `yaml:"size_efficiency" json:"size_efficiency"`
}

type SizeEfficiencyConfig struct {
	Mode               string     `yaml:"mode" json:"mode"`
	TargetMBMovie      [2]float64 `yaml:"target_mb_movie,omitempty" json:"target_mb_movie,omitempty"`
	TargetMBEpisode    [2]float64 `yaml:"target_mb_episode,omitempty" json:"target_mb_episode,omitempty"`
	TargetMBSeasonPack [2]float64 `yaml:"target_mb_season_pack,omitempty" json:"target_mb_season_pack,omitempty"`
}

// IfFoundConfig defines Tier 3 (Crowd Refinement / Tie-Breaker) rules.
type IfFoundConfig struct {
	Order              []string `yaml:"order" json:"order"`
	PreferredGroups    []string `yaml:"preferred_groups,omitempty" json:"preferred_groups,omitempty"`
	SeederBucketFactor float64  `yaml:"seeder_bucket_factor,omitempty" json:"seeder_bucket_factor,omitempty"`
}

// PruneConfig defines Phase 3 opt-in physical pruning guardrails.
type PruneConfig struct {
	ConfirmDestructive        bool     `yaml:"confirm_destructive" json:"confirm_destructive"`
	MinAge                    Duration `yaml:"min_age" json:"min_age"`
	BatchSize                 int      `yaml:"batch_size" json:"batch_size"`
	MaxDeleteFractionPerCycle float64  `yaml:"max_delete_fraction_per_cycle" json:"max_delete_fraction_per_cycle"`
	TombstoneTTL              Duration `yaml:"tombstone_ttl" json:"tombstone_ttl"`
	ReviveSlotOnWinnerDeath   bool     `yaml:"revive_slot_on_winner_death" json:"revive_slot_on_winner_death"`
}

// LoadProfileFile reads and strictly validates a declarative LEAN MODE YAML file.
func LoadProfileFile(path string) (ProfileConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ProfileConfig{}, fmt.Errorf("leanmode: read profile %q: %w", path, err)
	}
	return ParseProfile(raw)
}

// ParseProfile strictly decodes YAML (rejecting unknown keys), applies safe
// defaults, normalizes enum tokens, and validates all invariants.
func ParseProfile(raw []byte) (ProfileConfig, error) {
	var cfg ProfileConfig
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return ProfileConfig{}, fmt.Errorf("leanmode: decode yaml: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return ProfileConfig{}, err
	}
	return cfg, nil
}

func (c *ProfileConfig) applyDefaults() {
	if c.Version == 0 {
		c.Version = SchemaVersion
	}
	if c.Mode == "" {
		c.Mode = ModeObserve
	}
	c.Mode = strings.ToLower(strings.TrimSpace(c.Mode))
	if len(c.Scope.ContentTypes) == 0 {
		c.Scope.ContentTypes = []string{model.ContentTypeMovie.String(), model.ContentTypeTvShow.String()}
	}
	if c.Scope.SeasonPackPolicy == "" {
		c.Scope.SeasonPackPolicy = SeasonPackSeparateSlot
	}
	if c.Scope.MultiEpisodePolicy == "" {
		c.Scope.MultiEpisodePolicy = MultiEpisodeSeparateSlot
	}
	if c.Scope.UnlinkedPolicy == "" {
		c.Scope.UnlinkedPolicy = UnlinkedPassThrough
	}
	if c.Scope.NonVideoPolicy == "" {
		c.Scope.NonVideoPolicy = NonVideoPassThrough
	}
	if c.Resilience.UnknownSeedersGrace == 0 {
		c.Resilience.UnknownSeedersGrace = Duration(7 * 24 * time.Hour)
	}
	if c.Resilience.Hysteresis.MinSeederRatio == 0 {
		c.Resilience.Hysteresis.MinSeederRatio = 1.5
	}
	if c.Preferred.RankingMode == "" {
		c.Preferred.RankingMode = RankingModeWeighted
	}
	if c.Preferred.Weights == (PreferredWeights{}) {
		c.Preferred.Weights = PreferredWeights{
			Resolution:     300,
			Source:         220,
			Codec:          120,
			HDR:            80,
			Audio:          70,
			Group:          60,
			SizeEfficiency: 50,
		}
	}
	if c.Preferred.SizeEfficiency.Mode == "" {
		c.Preferred.SizeEfficiency.Mode = SizeEffNone
	}
	if len(c.IfFound.Order) == 0 {
		c.IfFound.Order = []string{
			TieBreakRepackProper,
			TieBreakPreferredGroup,
			TieBreakHighestSeeders,
			TieBreakCleanSingleMediaFile,
			TieBreakNewestVerified,
		}
	}
	if c.Prune.MinAge == 0 {
		c.Prune.MinAge = Duration(72 * time.Hour)
	}
	if c.Prune.BatchSize == 0 {
		c.Prune.BatchSize = 500
	}
	if c.Prune.MaxDeleteFractionPerCycle == 0 {
		c.Prune.MaxDeleteFractionPerCycle = 0.25
	}
	if c.Prune.TombstoneTTL == 0 {
		c.Prune.TombstoneTTL = Duration(90 * 24 * time.Hour)
	}
}

// Validate checks every field and compiles bounded RE2 patterns.
func (c *ProfileConfig) Validate() error {
	if c.Version != SchemaVersion {
		return fmt.Errorf("leanmode: unsupported schema version %d (expected %d)", c.Version, SchemaVersion)
	}
	if !ProfileIDPattern.MatchString(c.ID) {
		return fmt.Errorf("leanmode: invalid profile id %q (must match %s)", c.ID, ProfileIDPattern.String())
	}
	switch c.Mode {
	case ModeObserve, ModeServe, ModePrune:
	default:
		return fmt.Errorf("leanmode: invalid mode %q (expected observe, serve, or prune)", c.Mode)
	}
	for i, ct := range c.Scope.ContentTypes {
		norm := strings.ToLower(strings.TrimSpace(ct))
		if norm != "movie" && norm != "tv_show" {
			return fmt.Errorf("leanmode: unsupported scope.content_types entry %q (only movie and tv_show are slotted)", ct)
		}
		c.Scope.ContentTypes[i] = norm
	}
	switch c.Scope.SeasonPackPolicy {
	case SeasonPackSeparateSlot, SeasonPackPreferPack, SeasonPackPreferIndividual:
	default:
		return fmt.Errorf("leanmode: invalid scope.season_pack_policy %q", c.Scope.SeasonPackPolicy)
	}
	switch c.Scope.MultiEpisodePolicy {
	case MultiEpisodeSeparateSlot, MultiEpisodePreferIndividual:
	default:
		return fmt.Errorf("leanmode: invalid scope.multi_episode_policy %q", c.Scope.MultiEpisodePolicy)
	}
	switch c.Scope.UnlinkedPolicy {
	case UnlinkedPassThrough, UnlinkedExcludeFromServe, UnlinkedPruneAfterAge:
	default:
		return fmt.Errorf("leanmode: invalid scope.unlinked_policy %q", c.Scope.UnlinkedPolicy)
	}
	if c.Scope.UnlinkedPolicy == UnlinkedPruneAfterAge && c.Scope.UnlinkedPruneMinAge.Std() < 24*time.Hour {
		return fmt.Errorf("leanmode: scope.unlinked_prune_min_age must be >= 24h when unlinked_policy=prune_after_age")
	}
	switch c.Scope.NonVideoPolicy {
	case NonVideoPassThrough, NonVideoExcludeFromServe:
	default:
		return fmt.Errorf("leanmode: invalid scope.non_video_policy %q", c.Scope.NonVideoPolicy)
	}
	if c.Resilience.KeepRunnerUp < 0 || c.Resilience.KeepRunnerUp > 9 {
		return fmt.Errorf("leanmode: resilience.keep_runner_up must be 0..9 (got %d)", c.Resilience.KeepRunnerUp)
	}
	if c.Resilience.MinViableSeeders < 0 {
		return fmt.Errorf("leanmode: resilience.min_viable_seeders must be >= 0")
	}
	if c.Resilience.Hysteresis.MinPreferredDelta < 0 {
		return fmt.Errorf("leanmode: resilience.hysteresis.min_preferred_delta must be >= 0")
	}
	if c.Resilience.Hysteresis.MinSeederRatio < 1.0 {
		return fmt.Errorf("leanmode: resilience.hysteresis.min_seeder_ratio must be >= 1.0")
	}
	if err := c.validateRequired(); err != nil {
		return err
	}
	if err := c.validatePreferred(); err != nil {
		return err
	}
	if err := c.validateIfFound(); err != nil {
		return err
	}
	if c.Prune.BatchSize < 1 || c.Prune.BatchSize > 5000 {
		return fmt.Errorf("leanmode: prune.batch_size must be 1..5000 (got %d)", c.Prune.BatchSize)
	}
	if c.Prune.MaxDeleteFractionPerCycle <= 0 || c.Prune.MaxDeleteFractionPerCycle > 1.0 {
		return fmt.Errorf("leanmode: prune.max_delete_fraction_per_cycle must be in (0, 1.0]")
	}
	if c.Prune.MinAge.Std() < 0 || c.Prune.TombstoneTTL.Std() < 0 {
		return fmt.Errorf("leanmode: prune durations must be non-negative")
	}
	return nil
}

func normalizeResolution(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(s, "v")
	if s == "4k" || s == "uhd" {
		s = "2160p"
	}
	if s == "8k" {
		s = "4320p"
	}
	if s == "highest" || s == "highest_resolution" {
		return "highest", nil
	}
	if _, ok := resolutionRankMap[s]; !ok {
		return "", fmt.Errorf("unknown resolution %q", raw)
	}
	return s, nil
}

func (c *ProfileConfig) validateRequired() error {
	r := &c.Required
	if r.MinResolution != "" {
		norm, err := normalizeResolution(r.MinResolution)
		if err != nil || norm == "highest" {
			return fmt.Errorf("leanmode: required.min_resolution: %q invalid", r.MinResolution)
		}
		r.MinResolution = norm
	}
	if r.MaxResolution != "" {
		norm, err := normalizeResolution(r.MaxResolution)
		if err != nil || norm == "highest" {
			return fmt.Errorf("leanmode: required.max_resolution: %q invalid", r.MaxResolution)
		}
		r.MaxResolution = norm
	}
	if r.MinResolution != "" && r.MaxResolution != "" && resolutionRankMap[r.MinResolution] > resolutionRankMap[r.MaxResolution] {
		return fmt.Errorf("leanmode: required.min_resolution (%s) exceeds max_resolution (%s)", r.MinResolution, r.MaxResolution)
	}
	for i, res := range r.ResolutionsAllowed {
		norm, err := normalizeResolution(res)
		if err != nil || norm == "highest" {
			return fmt.Errorf("leanmode: required.resolutions_allowed[%d]: %q invalid", i, res)
		}
		r.ResolutionsAllowed[i] = norm
	}
	for i, src := range r.SourcesAllowed {
		u := normalizeSourceToken(src)
		if !validSources[u] {
			return fmt.Errorf("leanmode: required.sources_allowed[%d]: unknown source %q", i, src)
		}
		r.SourcesAllowed[i] = u
	}
	for i, src := range r.SourcesBlocked {
		u := normalizeSourceToken(src)
		if !validSources[u] {
			return fmt.Errorf("leanmode: required.sources_blocked[%d]: unknown source %q", i, src)
		}
		r.SourcesBlocked[i] = u
	}
	for i, codec := range r.CodecsAllowed {
		u := normalizeCodecToken(codec)
		if !validCodecs[u] {
			return fmt.Errorf("leanmode: required.codecs_allowed[%d]: unknown codec %q", i, codec)
		}
		r.CodecsAllowed[i] = u
	}
	for i, codec := range r.CodecsBlocked {
		u := normalizeCodecToken(codec)
		if !validCodecs[u] {
			return fmt.Errorf("leanmode: required.codecs_blocked[%d]: unknown codec %q", i, codec)
		}
		r.CodecsBlocked[i] = u
	}
	for i, mod := range r.ModifiersAllowed {
		u := strings.ToUpper(strings.TrimSpace(mod))
		if !validModifiers[u] {
			return fmt.Errorf("leanmode: required.modifiers_allowed[%d]: unknown modifier %q", i, mod)
		}
		r.ModifiersAllowed[i] = u
	}
	for i, mod := range r.ModifiersBlocked {
		u := strings.ToUpper(strings.TrimSpace(mod))
		if !validModifiers[u] {
			return fmt.Errorf("leanmode: required.modifiers_blocked[%d]: unknown modifier %q", i, mod)
		}
		r.ModifiersBlocked[i] = u
	}
	for i, h := range r.HDRRequired {
		u := normalizeHDRToken(h)
		if !validHDR[u] {
			return fmt.Errorf("leanmode: required.hdr_required[%d]: unknown hdr %q", i, h)
		}
		r.HDRRequired[i] = u
	}
	for i, h := range r.HDRBlocked {
		u := normalizeHDRToken(h)
		if !validHDR[u] {
			return fmt.Errorf("leanmode: required.hdr_blocked[%d]: unknown hdr %q", i, h)
		}
		r.HDRBlocked[i] = u
	}
	for i, a := range r.AudioFormatsAllowed {
		u := normalizeAudioToken(a)
		if !validAudio[u] {
			return fmt.Errorf("leanmode: required.audio_formats_allowed[%d]: unknown audio %q", i, a)
		}
		r.AudioFormatsAllowed[i] = u
	}
	for i, a := range r.AudioFeaturesRequired {
		u := normalizeAudioToken(a)
		if !validAudio[u] {
			return fmt.Errorf("leanmode: required.audio_features_required[%d]: unknown audio feature %q", i, a)
		}
		r.AudioFeaturesRequired[i] = u
	}
	for i, lang := range r.LanguagesRequired {
		r.LanguagesRequired[i] = strings.ToLower(strings.TrimSpace(lang))
	}
	for i, g := range r.ReleaseGroupsAllowed {
		r.ReleaseGroupsAllowed[i] = strings.ToLower(strings.TrimSpace(g))
	}
	for i, g := range r.ReleaseGroupsBlocked {
		r.ReleaseGroupsBlocked[i] = strings.ToLower(strings.TrimSpace(g))
	}
	if r.MinSeeders < 0 {
		return fmt.Errorf("leanmode: required.min_seeders must be >= 0")
	}
	if r.MinSizeMB < 0 || r.MaxSizeMB < 0 || r.MinSizeMBMovie < 0 || r.MaxSizeMBMovie < 0 ||
		r.MinSizeMBEpisode < 0 || r.MaxSizeMBEpisode < 0 || r.MinSizeMBSeasonPack < 0 || r.MaxSizeMBSeasonPack < 0 {
		return fmt.Errorf("leanmode: size limits must be non-negative")
	}
	if len(r.ExcludeTitlePatterns) > 16 {
		return fmt.Errorf("leanmode: required.exclude_title_patterns capped at 16 patterns")
	}
	c.compiledExcludePatterns = make([]*regexp.Regexp, 0, len(r.ExcludeTitlePatterns))
	for i, pat := range r.ExcludeTitlePatterns {
		if len(pat) == 0 || len(pat) > 128 {
			return fmt.Errorf("leanmode: required.exclude_title_patterns[%d] length must be 1..128", i)
		}
		re, err := regexp.Compile("(?i)" + pat)
		if err != nil {
			return fmt.Errorf("leanmode: required.exclude_title_patterns[%d]: %w", i, err)
		}
		c.compiledExcludePatterns = append(c.compiledExcludePatterns, re)
	}
	return nil
}

func (c *ProfileConfig) validatePreferred() error {
	p := &c.Preferred
	switch p.RankingMode {
	case RankingModeWeighted, RankingModeLexicographic:
	default:
		return fmt.Errorf("leanmode: invalid preferred.ranking_mode %q", p.RankingMode)
	}
	if p.EquivalenceBand < 0 {
		return fmt.Errorf("leanmode: preferred.equivalence_band must be >= 0")
	}
	for i, res := range p.ResolutionOrder {
		norm, err := normalizeResolution(res)
		if err != nil {
			return fmt.Errorf("leanmode: preferred.resolution_order[%d]: %w", i, err)
		}
		p.ResolutionOrder[i] = norm
	}
	for i, src := range p.SourceOrder {
		u := normalizeSourceToken(src)
		if !validSources[u] {
			return fmt.Errorf("leanmode: preferred.source_order[%d]: unknown source %q", i, src)
		}
		p.SourceOrder[i] = u
	}
	for i, codec := range p.CodecOrder {
		u := normalizeCodecToken(codec)
		if !validCodecs[u] {
			return fmt.Errorf("leanmode: preferred.codec_order[%d]: unknown codec %q", i, codec)
		}
		p.CodecOrder[i] = u
	}
	for i, h := range p.HDROrder {
		u := normalizeHDRToken(h)
		if !validHDR[u] {
			return fmt.Errorf("leanmode: preferred.hdr_order[%d]: unknown hdr %q", i, h)
		}
		p.HDROrder[i] = u
	}
	for i, a := range p.AudioOrder {
		u := normalizeAudioToken(a)
		if !validAudio[u] {
			return fmt.Errorf("leanmode: preferred.audio_order[%d]: unknown audio %q", i, a)
		}
		p.AudioOrder[i] = u
	}
	for i, g := range p.PreferredGroups {
		p.PreferredGroups[i] = strings.ToLower(strings.TrimSpace(g))
	}
	switch p.SizeEfficiency.Mode {
	case SizeEffNone, SizeEffTargetWindow, SizeEffPreferSmaller, SizeEffPreferLarger:
	default:
		return fmt.Errorf("leanmode: invalid preferred.size_efficiency.mode %q", p.SizeEfficiency.Mode)
	}
	return nil
}

func (c *ProfileConfig) validateIfFound() error {
	for i, tb := range c.IfFound.Order {
		norm := strings.ToLower(strings.TrimSpace(tb))
		if !validTieBreakers[norm] {
			return fmt.Errorf("leanmode: if_found.order[%d]: unknown tie-breaker %q", i, tb)
		}
		c.IfFound.Order[i] = norm
	}
	for i, g := range c.IfFound.PreferredGroups {
		c.IfFound.PreferredGroups[i] = strings.ToLower(strings.TrimSpace(g))
	}
	return nil
}

// ConfigDigest returns the stable SHA-256 hex digest of the normalized profile.
// Changing any rule produces a new digest, automatically expiring old tombstones.
func (c ProfileConfig) ConfigDigest() string {
	raw, _ := json.Marshal(c)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func normalizeSourceToken(raw string) string {
	s := strings.ToUpper(strings.TrimSpace(raw))
	s = strings.NewReplacer("-", "", "_", "", " ", "", ".", "").Replace(s)
	switch s {
	case "TS":
		return "TELESYNC"
	case "TC":
		return "TELECINE"
	case "WP":
		return "WORKPRINT"
	case "BDREMUX":
		return "REMUX"
	case "HDTV":
		return "TV"
	case "WEB":
		return "WEBRIP"
	default:
		return s
	}
}

func normalizeCodecToken(raw string) string {
	s := strings.ToUpper(strings.TrimSpace(raw))
	s = strings.NewReplacer(".", "", "-", "", "_", "", " ", "").Replace(s)
	switch s {
	case "AVC":
		return "H264"
	case "H265":
		return "HEVC"
	default:
		return s
	}
}

func normalizeHDRToken(raw string) string {
	s := strings.ToUpper(strings.TrimSpace(raw))
	s = strings.NewReplacer("-", "_", " ", "_").Replace(s)
	switch s {
	case "DV", "DOVI", "DOLBYVISION":
		return "DOLBY_VISION"
	case "HDR10+", "HDR10PLUS":
		return "HDR10_PLUS"
	default:
		return s
	}
}

func normalizeAudioToken(raw string) string {
	s := strings.ToUpper(strings.TrimSpace(raw))
	s = strings.NewReplacer("-", "_", " ", "_").Replace(s)
	switch s {
	case "DDP", "DD+", "DDPLUS", "E_AC_3", "EAC_3":
		return "EAC3"
	case "DD", "AC_3":
		return "AC3"
	case "DTSX", "DTS_X":
		return "DTS_X"
	case "TRUE_HD":
		return "TRUEHD"
	default:
		return s
	}
}
