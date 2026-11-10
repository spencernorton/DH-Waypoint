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

// asStored returns the torrent as the processor loads it: PostgreSQL fills the
// generated extension columns and the model sorts the files by path.
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
	_ = tor.AfterFind(nil)
	return tor
}

func multi(name string, paths ...string) model.Torrent {
	tor := model.Torrent{Name: name, FilesStatus: model.FilesStatusMulti}
	for _, p := range paths {
		tor.Files = append(tor.Files, model.TorrentFile{Path: p})
	}
	return tor
}

type parityCase struct {
	label   string
	torrent model.Torrent
}

// parityCases is a table of made-up torrents. A label carries the case
// number, never the name.
type parityCases []parityCase

func (c *parityCases) add(label string, tor model.Torrent) {
	*c = append(*c, parityCase{fmt.Sprintf("%s #%d", label, len(*c)), tor})
}

// shapes adds text in every shape a torrent is stored in.
func (c *parityCases) shapes(kind, text string) {
	c.add(kind+" no files", model.Torrent{Name: text})
	c.add(kind+" no files with extension", model.Torrent{Name: text + ".mkv"})
	c.add(kind+" over threshold", model.Torrent{Name: text, FilesStatus: model.FilesStatusOverThreshold})
	c.add(kind+" single file", model.Torrent{Name: text + ".mkv", FilesStatus: model.FilesStatusSingle})
	c.add(kind+" single file without extension", model.Torrent{Name: text, FilesStatus: model.FilesStatusSingle})
	c.add(kind+" multi name", multi(text, "Sample Folder/readme.txt"))
	c.add(kind+" multi directory", multi("Sample Collection", text+"/clip 01.mp4"))
	c.add(kind+" multi file", multi("Sample Collection", "Sample Folder/"+text+".mp4", "Sample Folder/notes.txt"))
	c.add(kind+" joined across parts", multi(text, text))
	// Two files whose stored order is the reverse of their order in the
	// metadata: the halves of the text meet only once the files are sorted.
	for i, r := range text {
		if r == ' ' {
			c.add(kind+" split across two files", multi("Sample Collection", text[i+1:]+" 2.mp4", "0 "+text[:i]))
		}
	}
}

// check holds MatchesBannedKeyword to the rule on every case, before storage
// and as stored, and returns how many cases the rule matched. A failure
// prints the case, never the name.
func (c parityCases) check(t *testing.T, rule func(model.Torrent) bool) (matched int) {
	t.Helper()
	for _, pc := range c {
		stored := asStored(pc.torrent)
		want := rule(stored)
		if want {
			matched++
		}
		if got := MatchesBannedKeyword(pc.torrent); got != want {
			t.Errorf("%s: before storage MatchesBannedKeyword=%v, classifier rule=%v", pc.label, got, want)
		}
		if got := MatchesBannedKeyword(stored); got != want {
			t.Errorf("%s: as stored MatchesBannedKeyword=%v, classifier rule=%v", pc.label, got, want)
		}
	}
	return matched
}

// TestBannedKeywordParity holds MatchesBannedKeyword to the classifier's
// banned-keyword rule. A torrent that has not been stored yet must get the
// answer the classifier gives for its stored form, in every shape a torrent
// is stored in. The cases are built from the built-in list at run time, so
// no keyword is spelled here. TestMatchesBannedKeyword runs the same check
// over its listed names.
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

	var cases parityCases
	cases.shapes("clean", "Sample Holiday Clip 2019")
	for _, tok := range tokens {
		cases.shapes("token", "Sample "+tok+" Clip")
		cases.shapes("token in word", "Sample"+tok+"Clip")
		cases.add("extension single file", model.Torrent{Name: "Sample Clip." + tok, FilesStatus: model.FilesStatusSingle})
		cases.add("extension no files", model.Torrent{Name: "Sample Clip." + tok})
		cases.add("extension multi name", multi("Sample Clip."+tok, "readme.txt"))
		cases.add("extension multi file", multi("Sample Collection", "Sample Folder/clip."+tok))
		cases.add("extension upper case", multi("Sample Collection", "Sample Folder/clip."+strings.ToUpper(tok)))
	}

	matched := cases.check(t, rule)
	if matched == 0 || matched == len(cases) {
		t.Fatalf("the table proves nothing: %d of %d cases match", matched, len(cases))
	}
	t.Logf("%d cases, %d matched by the rule", len(cases), matched)
}
