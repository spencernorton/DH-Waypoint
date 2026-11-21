package leanmode

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestShippedPresetsValidateAndDefaultSafe(t *testing.T) {
	dir := filepath.Join("..", "..", "plugins", "lean-mode")
	names := []string{"1080p-lean.yml", "4k-remux-best.yml", "small-footprint-x265.yml"}
	digests := make(map[string]string, len(names))
	for _, name := range names {
		p := filepath.Join(dir, name)
		prof, err := LoadProfileFile(p)
		require.NoError(t, err, "preset %s must validate cleanly", name)
		require.False(t, prof.Enabled, "preset %s must ship disabled by default", name)
		require.Equal(t, ModeObserve, prof.Mode, "preset %s must default to observe mode", name)
		require.False(t, prof.Prune.ConfirmDestructive, "preset %s must keep confirm_destructive false", name)
		d := prof.ConfigDigest()
		require.Len(t, d, 64)
		digests[name] = d
	}
	require.NotEqual(t, digests["1080p-lean.yml"], digests["4k-remux-best.yml"])
	require.NotEqual(t, digests["1080p-lean.yml"], digests["small-footprint-x265.yml"])
}

func TestDiffTitleDecisionsTracksChangesEmptyAndFilled(t *testing.T) {
	now := time.Now().UTC()
	p1080, err := LoadProfileFile(filepath.Join("..", "..", "plugins", "lean-mode", "1080p-lean.yml"))
	require.NoError(t, err)
	p4k, err := LoadProfileFile(filepath.Join("..", "..", "plugins", "lean-mode", "4k-remux-best.yml"))
	require.NoError(t, err)

	h1080 := []byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}
	h4k := []byte{2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2}
	cands := []Candidate{
		{
			InfoHash:        h1080,
			Name:            "Synthetic.Film.2024.1080p.BluRay.x265-NTb",
			ContentType:     "movie",
			ContentSource:   "tmdb",
			ContentID:       "101",
			SizeBytes:       4_200_000_000,
			VideoResolution: "1080p",
			VideoSource:     "BluRay",
			VideoCodec:      "x265",
			ReleaseGroup:    "NTb",
			Seeders:         diffIntPtr(30),
			PublishedAt:     now.Add(-48 * time.Hour),
		},
		{
			InfoHash:        h4k,
			Name:            "Synthetic.Film.2024.2160p.UHD.BluRay.REMUX.DV.Atmos-FraMeSToR",
			ContentType:     "movie",
			ContentSource:   "tmdb",
			ContentID:       "101",
			SizeBytes:       62_000_000_000,
			VideoResolution: "2160p",
			VideoSource:     "BluRay",
			VideoCodec:      "HEVC",
			VideoModifier:   "REMUX",
			ReleaseGroup:    "FraMeSToR",
			Seeders:         diffIntPtr(25),
			PublishedAt:     now.Add(-48 * time.Hour),
		},
	}

	d1080 := ResolveTitle(p1080, "movie", "tmdb", "101", cands, nil, now)
	d4k := ResolveTitle(p4k, "movie", "tmdb", "101", cands, nil, now)

	rep := DiffReport{
		FromID:        p1080.ID,
		FromDigest:    p1080.ConfigDigest(),
		ToID:          p4k.ID,
		ToDigest:      p4k.ConfigDigest(),
		SampleChanges: []DiffSample{},
	}
	DiffTitleDecisions(d1080, d4k, &rep, 10)
	require.Equal(t, 1, rep.SlotsCompared)
	require.Equal(t, 1, rep.Changed)
	require.Equal(t, 0, rep.Unchanged)
	require.Len(t, rep.SampleChanges, 1)
	require.Equal(t, "changed", rep.SampleChanges[0].ChangeType)
	require.Contains(t, rep.SampleChanges[0].FromName, "1080p")
	require.Contains(t, rep.SampleChanges[0].ToName, "2160p")

	// Comparing identical decisions reports 100% unchanged and 0 sample changes.
	repSame := DiffReport{SampleChanges: []DiffSample{}}
	DiffTitleDecisions(d1080, d1080, &repSame, 10)
	require.Equal(t, 1, repSame.Unchanged)
	require.Equal(t, 0, repSame.Changed)
	require.Empty(t, repSame.SampleChanges)
}

func diffIntPtr(v int) *int { return &v }
