package processors

import (
	"encoding/json"
	"regexp"
	"slices"
	"testing"

	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
)

// The regex template keeps classifying the names its comments promise.
func TestRegexTemplateClassifiesItsExamples(t *testing.T) {
	f, err := ParsePluginFile("../../plugins/regex-classifier.plugin.yml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg regexWeightsConfig
	if err := json.Unmarshal(f.Write().Configuration, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, rule := range cfg.Rules {
		// The driver skips a rule whose pattern does not compile, so check here.
		if _, err := regexp.Compile(rule.Pattern); err != nil {
			t.Fatalf("rule %s: %v", rule.ID, err)
		}
	}
	for _, tc := range []struct {
		name        string
		files       []string
		contentType string
		tags        []string
	}{
		{"Synthetic.Show.S01E02.1080p.WEB-DL.x264-GRP", nil, "tv_show", nil},
		{"Synthetic.Film.2024.2160p.UHD.BluRay.REMUX-GRP", nil, "movie", []string{"remux"}},
		{"Synthetic Album", []string{"Synthetic Album/01 - Overture.flac"}, "music", []string{"lossless"}},
		{"Synthetic Reading Pack", []string{"Synthetic Reading Pack/book.epub"}, "ebook", nil},
		{"Synthetic Notes Bundle", []string{"notes.txt"}, "", nil},
	} {
		j := v1.Job{Configuration: f.Write().Configuration, Input: v1.Input{Name: tc.name}}
		for _, p := range tc.files {
			j.Input.Files = append(j.Input.Files, v1.File{Path: p})
		}
		r, err := (&Store{}).runRegexWeights(j)
		if err != nil {
			t.Fatal(err)
		}
		got, tags := "", []string(nil)
		if r.Proposal != nil {
			got, tags = r.Proposal.ContentType, r.Proposal.AddTags
		}
		if got != tc.contentType || !slices.Equal(tags, tc.tags) {
			t.Fatalf("%s: got %q %v", tc.name, got, tags)
		}
	}
}
