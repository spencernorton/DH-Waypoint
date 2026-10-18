package processor

import (
	"sort"
	"strings"

	"github.com/spencernorton/DH-Waypoint/internal/classifier/classification"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/contentfilter"
	"github.com/spencernorton/DH-Waypoint/internal/model"
)

// torrentToFilterInput builds a fully-populated contentfilter.Input
// for the post-classifier hook. Unlike dhtcrawler's
// metaInfoToFilterInput (pre-classifier), this one threads the
// classifier's emitted ContentType + Languages into the input so the
// filter's LLM tier (Decide) can run on the residual cohort —
// Latin-script titles where TMDB couldn't set a language tag.
//
// Picking the primary file: largest by size with a non-empty
// extension. Single-file torrents (FilesStatus=single, Files often
// empty in this preload window) fall back to torrent.Extension.
func torrentToFilterInput(t model.Torrent, cl classification.Result) contentfilter.Input {
	allExtsSet := make(map[string]struct{}, len(t.Files))

	add := func(s string) string {
		ext := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), ".")
		if ext != "" {
			allExtsSet[ext] = struct{}{}
		}
		return ext
	}

	primaryExt := ""
	var primarySize uint

	for _, f := range t.Files {
		ext := ""
		if f.Extension.Valid {
			ext = add(f.Extension.String)
		}
		// Some old rows have a non-NULL but malformed Extension; fall
		// back to the basename of Path so we still get a primary-by-size
		// signal. matches metaInfoToFilterInput's tolerance for
		// slash/backslash paths.
		if ext == "" {
			base := f.Path
			if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
				base = base[i+1:]
			}
			if i := strings.LastIndex(base, "."); i >= 0 {
				ext = add(base[i+1:])
			}
		}
		if ext == "" {
			continue
		}
		if f.Size > primarySize {
			primarySize = f.Size
			primaryExt = ext
		}
	}

	// Single-file torrent: Files may not be hydrated by this point in
	// the preload graph; fall back to the torrent-level Extension.
	if primaryExt == "" && t.Extension.Valid {
		primaryExt = add(t.Extension.String)
	}

	allExts := make([]string, 0, len(allExtsSet))
	for e := range allExtsSet {
		allExts = append(allExts, e)
	}
	sort.Strings(allExts)

	contentType := ""
	if cl.ContentType.Valid {
		contentType = string(cl.ContentType.ContentType)
	}

	languages := make([]string, 0, len(cl.Languages))
	for lang := range cl.Languages {
		languages = append(languages, string(lang))
	}
	sort.Strings(languages)

	fileNames := make([]string, 0, len(t.Files))
	for _, f := range t.Files {
		if f.Path != "" {
			fileNames = append(fileNames, f.Path)
		}
	}

	in := contentfilter.Input{
		Private:          t.Private,
		Title:            t.Name,
		PrimaryExtension: primaryExt,
		AllExtensions:    allExts,
		ContentType:      contentType,
		Languages:        languages,
		FileNames:        fileNames,
	}
	if cl.BaseTitle.Valid {
		in.ReleaseTitle = cl.BaseTitle.String
	}
	if cl.Content != nil {
		in.ContentTitle = cl.Content.Title
		if cl.Content.OriginalTitle.Valid {
			in.OriginalTitle = cl.Content.OriginalTitle.String
		}
		if cl.Content.OriginalLanguage.Valid {
			in.OriginalLanguage = string(cl.Content.OriginalLanguage.Language)
		}
	}
	return in
}
