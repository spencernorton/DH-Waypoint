//go:build !race

package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInferReleaseAttributesZeroAllocOnPlainRelease(t *testing.T) {
	// Warm regexp sync.Pool entries once before measuring steady-state allocs.
	_ = InferReleaseAttributes("Plain.Movie.2024.1080p.BluRay.mkv", "1080p.BluRay")
	allocs := testing.AllocsPerRun(200, func() {
		if got := InferReleaseAttributes("Plain.Movie.2024.1080p.BluRay.mkv", "1080p.BluRay"); got != nil {
			t.Fatalf("unexpected attributes: %+v", got)
		}
	})
	require.Equal(t, float64(0), allocs, "plain release without claims must not allocate or compile regexes")
}

func BenchmarkInferReleaseAttributes(b *testing.B) {
	b.Run("plain", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = InferReleaseAttributes("Plain.Movie.2024.1080p.BluRay.mkv", "1080p.BluRay")
		}
	})
	b.Run("rich", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = InferReleaseAttributes("Amber.Signal.2025.mkv", "2160p.HEVC.DV.HDR10+.TrueHD.7.1.Atmos.PROPER.REPACK.x265")
		}
	})
}
