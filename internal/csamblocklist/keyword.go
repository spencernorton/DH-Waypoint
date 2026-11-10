package csamblocklist

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/spencernorton/DH-Waypoint/internal/classifier"
	"github.com/spencernorton/DH-Waypoint/internal/keywords"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protobuf"
)

// bannedKeywords is the built-in keywords.banned group, compiled by the
// compiler the classifier uses for it. Keywords an operator adds to that group
// are left out on purpose: only the built-in list keeps a torrent from being
// stored.
var bannedKeywords = sync.OnceValue(func() *regexp.Regexp {
	src, err := classifier.CoreSource()
	if err != nil {
		panic(fmt.Errorf("csamblocklist: built-in classifier definition: %w", err))
	}
	return keywords.MustNewRegexFromKeywords(src.Keywords["banned"]...)
})

// MatchesBannedKeyword reports whether the built-in banned-keyword rule
// matches t. It tests the text the classifier rule tests,
//
//	([torrent.baseName] + torrent.files.map(f, f.basePath)).join(' ')
//
// so a torrent checked before it is stored gets the answer the classifier
// gives once it is. TestBannedKeywordParity holds the two together.
func MatchesBannedKeyword(t model.Torrent) bool {
	return bannedKeywords().MatchString(bannedKeywordText(t))
}

// MentionsBannedKeyword is wider than MatchesBannedKeyword: it also tests the
// name and every file path as written, extensions included. It decides what
// may be copied into a record, never whether a torrent is dropped.
func MentionsBannedKeyword(t model.Torrent) bool {
	return MatchesBannedKeyword(t) || bannedKeywords().MatchString(t.Name) ||
		slices.ContainsFunc(t.Files, func(f model.TorrentFile) bool { return bannedKeywords().MatchString(f.Path) })
}

// SnapshotMatchesBannedKeyword applies MatchesBannedKeyword to a torrent kept
// as JSON: one torrents row and the array of its torrent_files rows.
func SnapshotMatchesBannedKeyword(torrentRow, fileRows []byte) (bool, error) {
	var snap struct {
		Name        string            `json:"name"`
		FilesStatus model.FilesStatus `json:"files_status"`
	}
	if err := json.Unmarshal(torrentRow, &snap); err != nil {
		return false, fmt.Errorf("read torrent snapshot: %w", err)
	}
	t := model.Torrent{Name: snap.Name, FilesStatus: snap.FilesStatus}
	var files []struct {
		Path string `json:"path"`
	}
	if len(fileRows) > 0 {
		if err := json.Unmarshal(fileRows, &files); err != nil {
			return false, fmt.Errorf("read files snapshot: %w", err)
		}
	}
	for _, f := range files {
		t.Files = append(t.Files, model.TorrentFile{Path: f.Path})
	}
	return MatchesBannedKeyword(t), nil
}

func bannedKeywordText(t model.Torrent) string {
	// extension is a generated column, so it is empty on a torrent that was
	// not read back from the database. Fill it the way the column does.
	if !t.Extension.Valid && t.FilesStatus == model.FilesStatusSingle {
		t.Extension = model.FileExtensionFromPath(t.Name)
	}
	files := make([]model.TorrentFile, len(t.Files))
	for i, f := range t.Files {
		if !f.Extension.Valid {
			f.Extension = model.FileExtensionFromPath(f.Path)
		}
		files[i] = f
	}
	// Stored files are read back sorted by path (model.Torrent.AfterFind),
	// and a keyword of several words can match across two neighbours.
	slices.SortFunc(files, func(a, b model.TorrentFile) int { return strings.Compare(a.Path, b.Path) })
	t.Files = files

	pt := protobuf.NewTorrent(t)
	parts := make([]string, 0, 1+len(pt.Files))
	parts = append(parts, pt.BaseName)
	for _, f := range pt.Files {
		parts = append(parts, f.BasePath)
	}
	return strings.Join(parts, " ")
}
