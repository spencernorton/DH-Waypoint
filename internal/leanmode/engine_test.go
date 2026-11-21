package leanmode

import (
	"bytes"
	"encoding/hex"
	"math/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/stretchr/testify/require"
)

func intPtr(v int) *int              { return &v }
func timePtr(v time.Time) *time.Time { return &v }

func mkHash(b byte) []byte {
	h := make([]byte, 20)
	for i := range h {
		h[i] = b
	}
	return h
}

func TestBundledPresetsValidateStrictly(t *testing.T) {
	presets := []string{
		"1080p-lean.yml",
		"4k-remux-best.yml",
		"small-footprint-x265.yml",
	}
	digests := map[string]bool{}
	for _, name := range presets {
		path := filepath.Join("..", "..", "plugins", "lean-mode", name)
		prof, err := LoadProfileFile(path)
		require.NoError(t, err, "preset %s should validate cleanly", name)
		require.False(t, prof.Enabled, "upstream preset %s must ship disabled by default", name)
		require.Equal(t, ModeObserve, prof.Mode, "upstream preset %s must default to observe mode", name)
		require.False(t, prof.Prune.ConfirmDestructive, "upstream preset %s must never pre-confirm destructive prune", name)
		d := prof.ConfigDigest()
		require.Len(t, d, 64)
		require.False(t, digests[d], "each preset must have a distinct digest")
		digests[d] = true
	}
}

func TestParseProfileRejectsUnknownFieldsAndInvalidConfig(t *testing.T) {
	_, err := ParseProfile([]byte("version: 1\nid: test\nunknown_field: true\n"))
	require.Error(t, err)

	_, err = ParseProfile([]byte("version: 1\nid: Bad ID!\n"))
	require.Error(t, err)

	_, err = ParseProfile([]byte("version: 1\nid: test\nrequired:\n  min_resolution: 2160p\n  max_resolution: 720p\n"))
	require.Error(t, err)

	_, err = ParseProfile([]byte("version: 1\nid: test\nmode: nuclear\n"))
	require.Error(t, err)

	_, err = ParseProfile([]byte("version: 1\nid: test\nrequired:\n  exclude_title_patterns: ['[unclosed']\n"))
	require.Error(t, err)
}

func TestThreeTierEngineRequiredPreferredIfFound(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	prof, err := LoadProfileFile(filepath.Join("..", "..", "plugins", "lean-mode", "1080p-lean.yml"))
	require.NoError(t, err)
	prof.Required.RequireArmigerApproved = true
	prof.Required.RequirePayloadProof = true
	require.NoError(t, prof.Validate())

	candidates := []Candidate{
		{
			// Disqualified: CAM source
			InfoHash: mkHash(0x01), Name: "Movie.2025.1080p.CAM.x264-BAD",
			ContentType: "movie", ContentSource: "tmdb", ContentID: "100",
			VideoResolution: "1080p", VideoSource: "CAM", VideoCodec: "x264",
			SizeBytes: 2 << 30, Seeders: intPtr(500), ArmigerApproved: true, PayloadProofActive: true,
			CreatedAt: now.Add(-48 * time.Hour),
		},
		{
			// Disqualified: missing PayloadProof
			InfoHash: mkHash(0x02), Name: "Movie.2025.1080p.BluRay.x265-NTb",
			ContentType: "movie", ContentSource: "tmdb", ContentID: "100",
			VideoResolution: "1080p", VideoSource: "BluRay", VideoCodec: "x265", ReleaseGroup: "NTb",
			SizeBytes: 4 << 30, Seeders: intPtr(120), ArmigerApproved: true, PayloadProofActive: false,
			CreatedAt: now.Add(-48 * time.Hour),
		},
		{
			// Disqualified: hardcoded subs pattern
			InfoHash: mkHash(0x03), Name: "Movie.2025.1080p.WEBDL.HCSUB.x264-SUB",
			ContentType: "movie", ContentSource: "tmdb", ContentID: "100",
			VideoResolution: "1080p", VideoSource: "WEBDL", VideoCodec: "x264",
			SizeBytes: 3 << 30, Seeders: intPtr(80), ArmigerApproved: true, PayloadProofActive: true,
			CreatedAt: now.Add(-48 * time.Hour),
		},
		{
			// Eligible lower tier: 720p WEBRip
			InfoHash: mkHash(0x04), Name: "Movie.2025.720p.WEBRip.x264-LOW",
			ContentType: "movie", ContentSource: "tmdb", ContentID: "100",
			VideoResolution: "720p", VideoSource: "WEBRip", VideoCodec: "x264", ReleaseGroup: "LOW",
			SizeBytes: 1500 << 20, Seeders: intPtr(300), ArmigerApproved: true, PayloadProofActive: true,
			CreatedAt: now.Add(-48 * time.Hour),
		},
		{
			// Top band candidate A: 1080p BluRay x265 NTb (no REPACK)
			InfoHash: mkHash(0x05), Name: "Movie.2025.1080p.BluRay.x265.HDR10.Atmos-NTb",
			ContentType: "movie", ContentSource: "tmdb", ContentID: "100",
			VideoResolution: "1080p", VideoSource: "BluRay", VideoCodec: "x265", ReleaseGroup: "NTb",
			HDRFormats: []string{"HDR10"}, AudioFeatures: []string{"ATMOS"},
			SizeBytes: 4500 << 20, Seeders: intPtr(60), ArmigerApproved: true, PayloadProofActive: true,
			PayloadProofVerifiedAt: timePtr(now.Add(-2 * time.Hour)),
			FilesStatus:            "single", VideoFileCount: 1, CreatedAt: now.Add(-48 * time.Hour),
		},
		{
			// Top band candidate B: identical quality + REPACK -> Tier 3 if_found wins!
			InfoHash: mkHash(0x06), Name: "Movie.2025.1080p.BluRay.REPACK.x265.HDR10.Atmos-NTb",
			ContentType: "movie", ContentSource: "tmdb", ContentID: "100",
			VideoResolution: "1080p", VideoSource: "BluRay", VideoCodec: "x265", ReleaseGroup: "NTb",
			HDRFormats: []string{"HDR10"}, AudioFeatures: []string{"ATMOS"}, Revisions: []string{"REPACK"},
			SizeBytes: 4600 << 20, Seeders: intPtr(45), ArmigerApproved: true, PayloadProofActive: true,
			PayloadProofVerifiedAt: timePtr(now.Add(-1 * time.Hour)),
			FilesStatus:            "single", VideoFileCount: 1, CreatedAt: now.Add(-36 * time.Hour),
		},
	}

	dec := EvaluateSlot(prof, "movie:tmdb:100", SlotKindMovie, 0, candidates, nil, now)
	require.Len(t, dec.Disqualified, 3)
	require.Len(t, dec.Winners, 2) // rank 1 + keep_runner_up=1
	require.Equal(t, mkHash(0x06), dec.Winners[0].Candidate.InfoHash, "REPACK should win Tier 3 tie-break")
	require.Equal(t, 1, dec.Winners[0].KeptRank)
	require.Equal(t, mkHash(0x05), dec.Winners[1].Candidate.InfoHash, "non-REPACK should stay as rank-2 warm runner-up")
	require.Equal(t, 2, dec.Winners[1].KeptRank)
	require.Len(t, dec.Losers, 1)
	require.Equal(t, mkHash(0x04), dec.Losers[0].Candidate.InfoHash)
	require.Equal(t, "dominated", dec.Losers[0].PruneReason)
}

func TestDeterminismAcross100ShuffledPermutations(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	prof, err := LoadProfileFile(filepath.Join("..", "..", "plugins", "lean-mode", "1080p-lean.yml"))
	require.NoError(t, err)

	base := make([]Candidate, 40)
	resolutions := []string{"720p", "1080p", "2160p"}
	sources := []string{"BluRay", "WEBDL", "WEBRip"}
	codecs := []string{"x265", "x264", "HEVC"}
	groups := []string{"ntb", "flux", "qxr", "other"}
	for i := range base {
		base[i] = Candidate{
			InfoHash:        mkHash(byte(i + 1)),
			Name:            "Show.S01E01.Release",
			ContentType:     "tv_show",
			ContentSource:   "tmdb",
			ContentID:       "42",
			VideoResolution: resolutions[i%len(resolutions)],
			VideoSource:     sources[(i/3)%len(sources)],
			VideoCodec:      codecs[(i/9)%len(codecs)],
			ReleaseGroup:    groups[i%len(groups)],
			SizeBytes:       uint64(400+i*60) << 20,
			Seeders:         intPtr(5 + (i%7)*3),
			CreatedAt:       now.Add(-96 * time.Hour),
		}
	}

	ref := EvaluateSlot(prof, "tv_show:tmdb:42:S01E01", SlotKindEpisode, 1, base, nil, now)
	require.Len(t, ref.Winners, 2)
	refW1 := hex.EncodeToString(ref.Winners[0].Candidate.InfoHash)
	refW2 := hex.EncodeToString(ref.Winners[1].Candidate.InfoHash)

	rng := rand.New(rand.NewSource(1337))
	for iter := 0; iter < 100; iter++ {
		perm := append([]Candidate(nil), base...)
		rng.Shuffle(len(perm), func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
		got := EvaluateSlot(prof, "tv_show:tmdb:42:S01E01", SlotKindEpisode, 1, perm, nil, now)
		require.Len(t, got.Winners, 2)
		require.Equal(t, refW1, hex.EncodeToString(got.Winners[0].Candidate.InfoHash))
		require.Equal(t, refW2, hex.EncodeToString(got.Winners[1].Candidate.InfoHash))
	}
}

func TestHysteresisAndRepackOverrideAndSwarmDeathFailover(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	prof, err := LoadProfileFile(filepath.Join("..", "..", "plugins", "lean-mode", "1080p-lean.yml"))
	require.NoError(t, err)

	prof.IfFound.SeederBucketFactor = 0 // test continuous log1p seeder tiebreak against 1.5x hysteresis
	incHash := mkHash(0x20)
	chalHash := mkHash(0x10)
	inc := Candidate{
		InfoHash: incHash, Name: "Movie.2025.1080p.BluRay.x265-NTb",
		ContentType: "movie", ContentSource: "tmdb", ContentID: "55",
		VideoResolution: "1080p", VideoSource: "BluRay", VideoCodec: "x265", ReleaseGroup: "NTb",
		SizeBytes: 4000 << 20, Seeders: intPtr(20), SeedersAuthoritative: true,
		CreatedAt: now.Add(-100 * time.Hour),
	}
	// Marginal seeder edge (25 vs 20 = 1.25x < 1.5x hysteresis ratio) must NOT dethrone incumbent!
	marginal := inc
	marginal.InfoHash = chalHash
	marginal.Seeders = intPtr(25)

	incState := &IncumbentState{
		InfoHash:       incHash,
		IncumbentSince: now.Add(-6 * time.Hour),
	}
	dec := EvaluateSlot(prof, "movie:tmdb:55", SlotKindMovie, 0, []Candidate{inc, marginal}, incState, now)
	require.True(t, dec.HysteresisHeld)
	require.True(t, bytes.Equal(incHash, dec.Winners[0].Candidate.InfoHash))

	// Adding REPACK immediately overrides incumbent hysteresis!
	repack := marginal
	repack.Revisions = []string{"REPACK"}
	decRepack := EvaluateSlot(prof, "movie:tmdb:55", SlotKindMovie, 0, []Candidate{inc, repack}, incState, now)
	require.False(t, decRepack.HysteresisHeld)
	require.True(t, bytes.Equal(chalHash, decRepack.Winners[0].Candidate.InfoHash))

	// Swarm death: incumbent drops to authoritative 0 seeders -> fails required min_seeders / viability,
	// runner-up promotes cleanly without losing the title!
	deadInc := inc
	deadInc.Seeders = intPtr(0)
	deadInc.SeedersAuthoritative = true
	decFailover := EvaluateSlot(prof, "movie:tmdb:55", SlotKindMovie, 0, []Candidate{deadInc, marginal}, incState, now)
	require.False(t, decFailover.NeedsSlotRevival)
	require.True(t, bytes.Equal(chalHash, decFailover.Winners[0].Candidate.InfoHash))

	// Total slot death (all candidates dead): triggers NeedsSlotRevival=true so tombstones can be cleared!
	deadChal := marginal
	deadChal.Seeders = intPtr(0)
	deadChal.SeedersAuthoritative = true
	decRevive := EvaluateSlot(prof, "movie:tmdb:55", SlotKindMovie, 0, []Candidate{deadInc, deadChal}, incState, now)
	require.True(t, decRevive.NeedsSlotRevival)
	require.Empty(t, decRevive.Winners)
}

func TestSeasonPackPolicies(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s1Pack := Candidate{
		InfoHash: mkHash(0xA1), Name: "Show.S01.1080p.BluRay.x265-QxR",
		ContentType: "tv_show", ContentSource: "tmdb", ContentID: "900",
		Episodes: model.Episodes{1: {}}, ReleaseGranularity: "season",
		VideoResolution: "1080p", VideoSource: "BluRay", VideoCodec: "x265", ReleaseGroup: "qxr",
		SizeBytes: 8000 << 20, Seeders: intPtr(40), CreatedAt: now.Add(-120 * time.Hour),
	}
	s1e1 := Candidate{
		InfoHash: mkHash(0xA2), Name: "Show.S01E01.1080p.WEBDL.x265-QxR",
		ContentType: "tv_show", ContentSource: "tmdb", ContentID: "900",
		Episodes: model.Episodes{1: {1: {}}}, ReleaseGranularity: "episode",
		VideoResolution: "1080p", VideoSource: "WEBDL", VideoCodec: "x265", ReleaseGroup: "qxr",
		SizeBytes: 600 << 20, Seeders: intPtr(25), CreatedAt: now.Add(-120 * time.Hour),
	}
	// Season 2 has no pack yet (currently airing) -> episode MUST survive even under prefer_pack!
	s2e1 := Candidate{
		InfoHash: mkHash(0xA3), Name: "Show.S02E01.1080p.WEBDL.x265-QxR",
		ContentType: "tv_show", ContentSource: "tmdb", ContentID: "900",
		Episodes: model.Episodes{2: {1: {}}}, ReleaseGranularity: "episode",
		VideoResolution: "1080p", VideoSource: "WEBDL", VideoCodec: "x265", ReleaseGroup: "qxr",
		SizeBytes: 650 << 20, Seeders: intPtr(30), CreatedAt: now.Add(-120 * time.Hour),
	}

	cands := []Candidate{s1Pack, s1e1, s2e1}

	// 1. separate_slot: all 3 keep their own winner slot
	pSep, err := LoadProfileFile(filepath.Join("..", "..", "plugins", "lean-mode", "small-footprint-x265.yml"))
	require.NoError(t, err)
	pSep.Scope.SeasonPackPolicy = SeasonPackSeparateSlot
	tdSep := ResolveTitle(pSep, "tv_show", "tmdb", "900", cands, nil, now)
	require.Len(t, tdSep.Slots, 3)
	for _, s := range tdSep.Slots {
		require.Len(t, s.Winners, 1)
	}

	// 2. prefer_pack: S01 pack subsumes S01E01, while S02E01 (no pack) stays alive!
	pPack := pSep
	pPack.Scope.SeasonPackPolicy = SeasonPackPreferPack
	tdPack := ResolveTitle(pPack, "tv_show", "tmdb", "900", cands, nil, now)
	winnersBySlot := map[string]int{}
	for _, s := range tdPack.Slots {
		winnersBySlot[s.SlotKey] = len(s.Winners)
	}
	require.Equal(t, 1, winnersBySlot["tv_show:tmdb:900:S01"])
	require.Equal(t, 0, winnersBySlot["tv_show:tmdb:900:S01E01"])
	require.Equal(t, 1, winnersBySlot["tv_show:tmdb:900:S02E01"])

	// 3. prefer_individual: S01E01 & S02E01 win, S01 pack subsumed!
	pInd := pSep
	pInd.Scope.SeasonPackPolicy = SeasonPackPreferIndividual
	tdInd := ResolveTitle(pInd, "tv_show", "tmdb", "900", cands, nil, now)
	winnersBySlot = map[string]int{}
	for _, s := range tdInd.Slots {
		winnersBySlot[s.SlotKey] = len(s.Winners)
	}
	require.Equal(t, 0, winnersBySlot["tv_show:tmdb:900:S01"])
	require.Equal(t, 1, winnersBySlot["tv_show:tmdb:900:S01E01"])
	require.Equal(t, 1, winnersBySlot["tv_show:tmdb:900:S02E01"])
}

func TestStoreActiveProfileRaceSafety(t *testing.T) {
	prof, err := LoadProfileFile(filepath.Join("..", "..", "plugins", "lean-mode", "1080p-lean.yml"))
	require.NoError(t, err)
	prof.Mode = ModePrune
	prof.Prune.ConfirmDestructive = true

	st := NewStore(nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if id%2 == 0 {
					st.SetActiveProfile(&prof, j%2 == 0)
				} else {
					_, _, _, _ = st.ActiveServingProfile()
					_, _, _ = st.ActivePruneIdentity()
				}
			}
		}(i)
	}
	wg.Wait()
}
