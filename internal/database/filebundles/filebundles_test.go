package filebundles

import (
	"testing"

	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
)

func TestDeduplicateFiles(t *testing.T) {
	in := []model.TorrentFile{
		{Index: 2, Path: "dir/b.MKV", Size: 20, Extension: model.NewNullString("wrong")},
		{Index: 0, Path: "dir/a.txt", Size: 10},
		{Index: 1, Path: "dir/a.txt", Size: 99},
		{Index: 2, Path: "dir/c", Size: 30, Extension: model.NewNullString("bin")},
		{Index: 0, Path: "readme", Size: 5},
	}
	got := DeduplicateFiles(in)
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4: %+v", len(got), got)
	}
	// Final order: Index then Path. Duplicate dir/a.txt keeps the first
	// (Index 0, Size 10) after stable Index sort.
	want := []model.TorrentFile{
		{Index: 0, Path: "dir/a.txt", Size: 10},
		{Index: 0, Path: "readme", Size: 5},
		{Index: 2, Path: "dir/b.MKV", Size: 20},
		{Index: 2, Path: "dir/c", Size: 30},
	}
	for i, w := range want {
		if got[i].Index != w.Index || got[i].Path != w.Path || got[i].Size != w.Size {
			t.Errorf("[%d] = {Index:%d Path:%q Size:%d}, want {Index:%d Path:%q Size:%d}",
				i, got[i].Index, got[i].Path, got[i].Size, w.Index, w.Path, w.Size)
		}
	}
	if !got[0].Extension.Valid || got[0].Extension.String != "txt" {
		t.Errorf("a.txt extension = %+v, want txt", got[0].Extension)
	}
	if got[1].Extension.Valid {
		t.Errorf("readme extension = %+v, want NULL", got[1].Extension)
	}
	if !got[2].Extension.Valid || got[2].Extension.String != "mkv" {
		t.Errorf("b.MKV extension = %+v, want recomputed lowercase mkv", got[2].Extension)
	}
	if got[3].Extension.Valid {
		t.Errorf("dir/c extension = %+v, want recomputed NULL (no extension)", got[3].Extension)
	}
	if in[0].Index != 2 || in[1].Index != 0 {
		t.Errorf("input mutated: %+v", in)
	}
}

func TestDeduplicateFilesEmpty(t *testing.T) {
	if got := DeduplicateFiles(nil); len(got) != 0 {
		t.Errorf("DeduplicateFiles(nil) = %+v", got)
	}
}

func TestParseWriteMode(t *testing.T) {
	for _, s := range []string{"compact", "dual", "legacy"} {
		mode, err := ParseWriteMode(s)
		if err != nil {
			t.Errorf("ParseWriteMode(%q): %v", s, err)
		} else if string(mode) != s {
			t.Errorf("ParseWriteMode(%q) = %q", s, mode)
		}
	}
	for _, s := range []string{"", "COMPACT", "both", "bundles"} {
		if _, err := ParseWriteMode(s); err == nil {
			t.Errorf("ParseWriteMode(%q) succeeded, want error", s)
		}
	}
}

func TestWriteModeFromEnv(t *testing.T) {
	t.Setenv("STORAGE_FILE_BUNDLES_WRITE", "")
	mode, err := WriteModeFromEnv()
	if err != nil || mode != ModeCompact {
		t.Errorf("empty env = %q, %v; want compact", mode, err)
	}
	t.Setenv("STORAGE_FILE_BUNDLES_WRITE", "dual")
	mode, err = WriteModeFromEnv()
	if err != nil || mode != ModeDual {
		t.Errorf("dual env = %q, %v; want dual", mode, err)
	}
	t.Setenv("STORAGE_FILE_BUNDLES_WRITE", "legacy")
	mode, err = WriteModeFromEnv()
	if err != nil || mode != ModeLegacy {
		t.Errorf("legacy env = %q, %v; want legacy", mode, err)
	}
	t.Setenv("STORAGE_FILE_BUNDLES_WRITE", "bogus")
	if _, err := WriteModeFromEnv(); err == nil {
		t.Error("bogus env succeeded, want error")
	}
}

func TestBuildBulkBundleUpsert(t *testing.T) {
	h1 := protocol.InfoHash{0x02}
	h2 := protocol.InfoHash{0x01}
	byHash := map[protocol.InfoHash][]model.TorrentFile{
		h1: {
			{InfoHash: h1, Index: 1, Path: "README", Size: 5},
			{InfoHash: h1, Index: 0, Path: "movie.mkv", Size: 100},
		},
		h2: {
			{InfoHash: h2, Index: 0, Path: "subs.srt", Size: 10},
		},
	}
	prepared := prepareBundles(byHash)
	if len(prepared) != 2 {
		t.Fatalf("len(prepared) = %d, want 2", len(prepared))
	}
	if prepared[0].hash != h2 || prepared[1].hash != h1 {
		t.Fatalf("prepared not deterministically sorted by info_hash")
	}
	sqlStr, args := buildBulkBundleUpsert(prepared)
	wantSQL := "INSERT INTO torrent_file_bundles(info_hash,indexes,paths,sizes,extensions) VALUES($1,$2,$3,$4,$5),($6,$7,$8,$9,$10) ON CONFLICT(info_hash) DO NOTHING"
	if sqlStr != wantSQL {
		t.Fatalf("sql = %q, want %q", sqlStr, wantSQL)
	}
	if len(args) != 10 {
		t.Fatalf("len(args) = %d, want 10", len(args))
	}
	exts1, ok := args[9].([]*string)
	if !ok || len(exts1) != 2 || exts1[0] == nil || *exts1[0] != "mkv" || exts1[1] != nil {
		t.Fatalf("extensions for h1 = %#v, want [\"mkv\", nil]", args[9])
	}
}
