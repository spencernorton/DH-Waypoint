package processor

import (
	"context"
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/classifier/classification"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/contentfilter"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
)

func file(path string, size uint) model.TorrentFile {
	return model.TorrentFile{Path: path, Size: size}
}

func TestBasicResultCategories(t *testing.T) {
	cases := []struct {
		name    string
		torrent model.Torrent
		want    string // "" means unknown (invalid content type)
	}{
		{"movie from files", model.Torrent{Name: "Some Film 2024", Size: 1000, Files: []model.TorrentFile{file("film.mkv", 900), file("subs.srt", 10)}}, "movie"},
		{"tv from episode pattern", model.Torrent{Name: "Some Show S01E02", Size: 1000, Files: []model.TorrentFile{file("show.s01e02.mkv", 900)}}, "tv_show"},
		{"music from dominant audio", model.Torrent{Name: "An Album", Size: 1000, Files: []model.TorrentFile{file("01.flac", 400), file("02.flac", 400), file("cover.jpg", 10)}}, "music"},
		{"audiobook from m4b", model.Torrent{Name: "A Book", Size: 1000, Files: []model.TorrentFile{file("book.m4b", 900)}}, "audiobook"},
		{"ebook", model.Torrent{Name: "A Novel", Size: 1000, Files: []model.TorrentFile{file("novel.epub", 900)}}, "ebook"},
		{"comic", model.Torrent{Name: "A Comic", Size: 1000, Files: []model.TorrentFile{file("issue.cbz", 900)}}, "comic"},
		{"software", model.Torrent{Name: "An App", Size: 1000, Files: []model.TorrentFile{file("setup.exe", 900)}}, "software"},
		{"minority video loses to audio", model.Torrent{Name: "Live Set", Size: 1000, Files: []model.TorrentFile{file("teaser.mkv", 100), file("set.mp3", 800)}}, "music"},
		{"no majority stays unknown", model.Torrent{Name: "Mixed Bag", Size: 1000, Files: []model.TorrentFile{file("a.mkv", 400), file("b.mp3", 400), file("c.txt", 200)}}, ""},
		{"unknown extension", model.Torrent{Name: "Mystery", Size: 1000, Files: []model.TorrentFile{file("data.zzz", 900)}}, ""},
		{"single file from name", model.Torrent{Name: "Lone Film 2024.mkv", Size: 1000}, "movie"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := basicResult(tc.torrent)
			gotType := ""
			if got.ContentType.Valid {
				gotType = got.ContentType.ContentType.String()
			}
			if gotType != tc.want {
				t.Fatalf("basicResult(%q) type = %q, want %q", tc.torrent.Name, gotType, tc.want)
			}
		})
	}
}

func TestBasicResultKeepsHintType(t *testing.T) {
	torrent := model.Torrent{Name: "Hinted", Size: 1000, Files: []model.TorrentFile{file("x.mkv", 900)}}
	torrent.Hint.ContentType = model.ContentTypeMusic
	got := basicResult(torrent)
	if !got.ContentType.Valid || got.ContentType.ContentType != model.ContentTypeMusic {
		t.Fatalf("hint type not preserved: %+v", got.ContentType)
	}
}

func TestRawResultOnlyAppliesHint(t *testing.T) {
	torrent := model.Torrent{Name: "Raw Film", Size: 1000, Files: []model.TorrentFile{file("film.mkv", 900)}}
	got := rawResult(torrent)
	if got.ContentType.Valid {
		t.Fatalf("raw mode inferred type %v from files, want unknown", got.ContentType.ContentType)
	}
	torrent.Hint.ContentType = model.ContentTypeMovie
	got = rawResult(torrent)
	if !got.ContentType.Valid || got.ContentType.ContentType != model.ContentTypeMovie {
		t.Fatalf("raw mode dropped hint: %+v", got.ContentType)
	}
}

func TestNormalizeModeDefaultsToBasic(t *testing.T) {
	if got := NewDefaultConfig().Mode; got != ModeBasic {
		t.Fatalf("configured default = %q, want basic", got)
	}
	if got := Mode("").Normalize(); got != ModeBasic {
		t.Fatalf("empty mode normalized to %q, want %q", got, ModeBasic)
	}
	for _, m := range []Mode{ModeFull, ModeBasic, ModeRaw} {
		if !m.Valid() {
			t.Fatalf("mode %q should be valid", m)
		}
	}
	if Mode("turbo").Valid() {
		t.Fatal("mode turbo should be invalid")
	}
}

func TestDefaultModeNeedsNoClassifierOrPlugin(t *testing.T) {
	hash := processTestHash(3)
	p, mock, blocker := newProcessTestProcessor(t,
		[]model.Torrent{processTestTorrent(hash, "kept movie 2026", "mkv")},
		processRunnerStub{run: func(model.Torrent) (classification.Result, error) {
			t.Fatal("default mode invoked the optional classifier")
			return classification.Result{}, nil
		}},
	)
	p.mode = NewDefaultConfig().Mode
	expectTorrentContentInsert(mock)
	if err := p.Process(context.Background(), MessageParams{InfoHashes: []protocol.InfoHash{hash}}); err != nil {
		t.Fatal(err)
	}
	if got := blocker.blockedHashes(); len(got) != 0 {
		t.Fatalf("default mode blocked metadata: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Raw mode must never invoke the CEL runner or the content filter: a torrent
// that both would delete is persisted untouched.
func TestProcessRawModeSkipsRunnerAndContentFilter(t *testing.T) {
	hash := processTestHash(3)
	runnerCalled := false
	p, mock, blocker := newProcessTestProcessor(t,
		[]model.Torrent{processTestTorrent(hash, "kept movie 2026", "mkv")},
		processRunnerStub{run: func(model.Torrent) (classification.Result, error) {
			runnerCalled = true
			return classification.Result{}, classification.ErrDeleteTorrent
		}},
	)
	p.mode = ModeRaw
	cfg := contentfilter.NewDefaultConfig()
	cfg.Enabled = true
	cfg.Enforce = true
	cfg.BlockedExtensions = []string{"mkv"}
	p.contentFilter = contentfilter.New(cfg)

	expectTorrentContentInsert(mock)
	if err := p.Process(context.Background(), MessageParams{InfoHashes: []protocol.InfoHash{hash}}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	if runnerCalled {
		t.Fatal("runner was invoked in raw mode")
	}
	if got := blocker.blockedHashes(); len(got) != 0 {
		t.Fatalf("blocked hashes = %v, want none", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}
