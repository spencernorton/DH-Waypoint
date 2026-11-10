package csamblocklist

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/classifier"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/classification"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/tmdb"
)

// bannedKeywordRule compiles the banned-keyword step of the built-in default
// workflow on its own and reports whether it deletes a torrent.
func bannedKeywordRule(t *testing.T) func(model.Torrent) bool {
	t.Helper()
	src, err := classifier.CoreSource()
	if err != nil {
		t.Fatal(err)
	}
	steps, _ := src.Workflows["default"].([]any)
	if len(steps) == 0 || !strings.Contains(fmt.Sprint(steps[0]), "matches(keywords.banned)") {
		t.Fatal("the first step of the built-in default workflow is no longer the banned-keyword rule; point this test at it")
	}
	src.Workflows = map[string]any{"default": steps[:1]}
	compiler, err := classifier.New(classifier.Params{
		Config:     classifier.NewDefaultConfig(),
		Search:     lazy.New(func() (search.Search, error) { return nil, nil }),
		TmdbClient: lazy.New(func() (tmdb.Client, error) { return nil, nil }),
	}).Compiler.Get()
	if err != nil {
		t.Fatal(err)
	}
	runner, err := compiler.Compile(src)
	if err != nil {
		t.Fatal(err)
	}
	return func(tor model.Torrent) bool {
		_, err := runner.Run(context.Background(), "default", nil, tor)
		if err != nil && !errors.Is(err, classification.ErrDeleteTorrent) {
			t.Fatal(err)
		}
		return err != nil
	}
}

// asStored fills the generated extension columns the way PostgreSQL does, so
// the classifier sees the torrent as the processor loads it.
func asStored(tor model.Torrent) model.Torrent {
	if tor.FilesStatus == model.FilesStatusSingle {
		tor.Extension = model.FileExtensionFromPath(tor.Name)
	}
	files := make([]model.TorrentFile, len(tor.Files))
	for i, f := range tor.Files {
		f.Extension = model.FileExtensionFromPath(f.Path)
		files[i] = f
	}
	tor.Files = files
	return tor
}

func multi(name string, paths ...string) model.Torrent {
	tor := model.Torrent{Name: name, FilesStatus: model.FilesStatusMulti}
	for _, p := range paths {
		tor.Files = append(tor.Files, model.TorrentFile{Path: p})
	}
	return tor
}

// TestBannedKeywordParity holds MatchesBannedKeyword to the classifier's
// banned-keyword rule. A torrent that has not been stored yet must get the
// answer the classifier gives for its stored form, in every shape a torrent
// is stored in. All names are made up; a failure prints the case, never the
// name.
func TestBannedKeywordParity(t *testing.T) {
	rule := bannedKeywordRule(t)

	// The plain tokens of the built-in list double as file extensions: the
	// rule tests base names, so an extension must not count.
	src, err := classifier.CoreSource()
	if err != nil {
		t.Fatal(err)
	}
	plain := regexp.MustCompile(`^[a-z0-9]+$`)
	var tokens []string
	for _, kw := range src.Keywords["banned"] {
		if plain.MatchString(kw) {
			tokens = append(tokens, kw)
		}
	}
	if len(tokens) == 0 {
		t.Fatal("the built-in banned list has no plain token to build cases from")
	}

	type testCase struct {
		label   string
		torrent model.Torrent
	}
	var cases []testCase
	add := func(label string, tor model.Torrent) {
		cases = append(cases, testCase{fmt.Sprintf("%s #%d", label, len(cases)), tor})
	}
	shapes := func(kind, text string) {
		add(kind+" no files", model.Torrent{Name: text})
		add(kind+" no files with extension", model.Torrent{Name: text + ".mkv"})
		add(kind+" over threshold", model.Torrent{Name: text, FilesStatus: model.FilesStatusOverThreshold})
		add(kind+" single file", model.Torrent{Name: text + ".mkv", FilesStatus: model.FilesStatusSingle})
		add(kind+" single file without extension", model.Torrent{Name: text, FilesStatus: model.FilesStatusSingle})
		add(kind+" multi name", multi(text, "Sample Folder/readme.txt"))
		add(kind+" multi directory", multi("Sample Collection", text+"/clip 01.mp4"))
		add(kind+" multi file", multi("Sample Collection", "Sample Folder/"+text+".mp4", "Sample Folder/notes.txt"))
		add(kind+" joined across parts", multi(text, text))
	}
	for _, name := range mustMatchNames {
		shapes("listed match", name)
	}
	for _, name := range mustNotMatchNames {
		shapes("listed clean", name)
	}
	for _, tok := range tokens {
		shapes("token", "Sample "+tok+" Clip")
		shapes("token in word", "Sample"+tok+"Clip")
		add("extension single file", model.Torrent{Name: "Sample Clip." + tok, FilesStatus: model.FilesStatusSingle})
		add("extension no files", model.Torrent{Name: "Sample Clip." + tok})
		add("extension multi name", multi("Sample Clip."+tok, "readme.txt"))
		add("extension multi file", multi("Sample Collection", "Sample Folder/clip."+tok))
		add("extension upper case", multi("Sample Collection", "Sample Folder/clip."+strings.ToUpper(tok)))
	}

	matched := 0
	for _, c := range cases {
		stored := asStored(c.torrent)
		want := rule(stored)
		if want {
			matched++
		}
		if got := MatchesBannedKeyword(c.torrent); got != want {
			t.Errorf("%s: before storage MatchesBannedKeyword=%v, classifier rule=%v", c.label, got, want)
		}
		if got := MatchesBannedKeyword(stored); got != want {
			t.Errorf("%s: as stored MatchesBannedKeyword=%v, classifier rule=%v", c.label, got, want)
		}
	}
	if matched == 0 || matched == len(cases) {
		t.Fatalf("the table proves nothing: %d of %d cases match", matched, len(cases))
	}
	t.Logf("%d cases, %d matched by the rule", len(cases), matched)
}
