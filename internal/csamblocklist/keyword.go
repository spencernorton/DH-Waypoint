package csamblocklist

import (
	"fmt"
	"regexp"
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
	t.Files = files

	pt := protobuf.NewTorrent(t)
	parts := make([]string, 0, 1+len(pt.Files))
	parts = append(parts, pt.BaseName)
	for _, f := range pt.Files {
		parts = append(parts, f.BasePath)
	}
	return strings.Join(parts, " ")
}
