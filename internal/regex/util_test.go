package regex_test

import (
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/regex"
	"github.com/stretchr/testify/assert"
)

func TestNormalizeString(t *testing.T) {
	t.Parallel()

	type parseTest struct {
		inputString    string
		expectedOutput string
	}

	parseTests := []parseTest{
		{
			inputString:    "Mission.Impossible 'quoted string' and \"double quoted string\" &&jF $$ q",
			expectedOutput: "mission impossible 'quoted string' and \"double quoted string\" jf q",
		},
	}

	for _, test := range parseTests {
		t.Run(test.inputString, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, test.expectedOutput, regex.NormalizeString(test.inputString))
		})
	}
}

func TestNormalizeSearchString(t *testing.T) {
	t.Parallel()

	type parseTest struct {
		inputString    string
		expectedOutput string
	}

	parseTests := []parseTest{
		{
			inputString:    "Mission.Impossible 'quoted string' and \"double quoted string\" &&jF $$ -weak q",
			expectedOutput: "mission impossible 'quoted string' and \"double quoted string\" jf -weak q",
		},
	}

	for _, test := range parseTests {
		t.Run(test.inputString, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, test.expectedOutput, regex.NormalizeSearchString(test.inputString))
		})
	}
}

func TestNormalizeStringUnicodeAndConcurrency(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 0, regex.WordTokenRegex().NumSubexp())

	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ... ---   ", ""},
		{"The.Matrix.1999.1080p.BluRay.x264", "the matrix 1999 1080p bluray x264"},
		{"Am\u00e9lie.2001.1080p", "am\u00e9lie 2001 1080p"},
		{"Ame\u0301lie.2001.1080p", "am\u00e9lie 2001 1080p"},
		{"Caf\u00e9.del.Mar", "caf\u00e9 del mar"},
		{"Cafe\u0301.del.Mar", "caf\u00e9 del mar"},
		{"\u010cesk\u00e9.Film.2024", "\u010desk\u00e9 film 2024"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, regex.NormalizeString(tc.in), tc.in)
	}

	// Concurrent stress for pooled NFD->NFC transformer under -race.
	done := make(chan struct{}, 8)
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 200 {
				assert.Equal(t, "am\u00e9lie 2001 1080p", regex.NormalizeString("Ame\u0301lie.2001.1080p"))
				assert.Equal(t, "the matrix 1999", regex.NormalizeString("The.Matrix.1999"))
			}
		}()
	}
	for range 8 {
		<-done
	}
}

func BenchmarkNormalizeString(b *testing.B) {
	b.Run("ascii", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = regex.NormalizeString("The.Matrix.1999.1080p.BluRay.REMUX.TrueHD.7.1.x265-GROUP.mkv")
		}
	})
	b.Run("unicode", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = regex.NormalizeString("Ame\u0301lie.2001.1080p.BluRay.DTS-HD.MA.5.1.mkv")
		}
	})
}
