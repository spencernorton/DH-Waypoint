package processor

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spencernorton/DH-Waypoint/internal/classifier/classification"
	"github.com/spencernorton/DH-Waypoint/internal/model"
)

// Mode selects how much deterministic processing the core applies to a
// torrent before optional processor services see it. It is configured by
// PROCESSING_MODE (or `processing.mode` in YAML config).
//
//   - full (default): the complete CEL classifier workflow, including
//     title parsing, metadata matching, and the deterministic content
//     filter when that filter is itself enabled.
//   - basic: no CEL workflow, no metadata providers, no network calls.
//     The core stamps only a coarse content type derived from file
//     extensions (and, for video, an episode pattern in the name).
//     Intended for installs where a processor service or custom
//     dataset performs all real classification and filtering.
//   - raw: the core persists metainfo exactly as discovered. Only an
//     operator/import-supplied hint is applied; content type stays
//     unknown until a processor proposes one. Nothing is deleted or
//     filtered by classification in this mode.
//
// Name policy admission and the CSAM pre-fetch blocklist are independent
// safety controls with their own toggles and are not affected by Mode.
type Mode string

const (
	ModeFull  Mode = "full"
	ModeBasic Mode = "basic"
	ModeRaw   Mode = "raw"
)

// Config is the `processing` configuration section.
type Config struct {
	Mode Mode `validate:"oneof=full basic raw"`
}

func NewDefaultConfig() Config {
	return Config{Mode: ModeFull}
}

func (m Mode) Valid() bool {
	switch m {
	case ModeFull, ModeBasic, ModeRaw:
		return true
	}
	return false
}

// Normalize maps the empty value (direct struct construction, older
// wiring) to ModeFull so existing behaviour is preserved unless an
// operator opts into a leaner mode.
func (m Mode) Normalize() Mode {
	if m == "" {
		return ModeFull
	}
	return m
}

// rawResult applies only the torrent's own hint (operator/import
// supplied facts). File evidence is never interpreted.
func rawResult(t model.Torrent) classification.Result {
	cl := classification.Result{}
	if !t.Hint.IsNil() {
		cl.ApplyHint(t.Hint)
	}
	return cl
}

var episodePattern = regexp.MustCompile(`(?i)(?:\bs\d{1,2}e\d{1,3}\b|\bseason\s*\d+\b|\bcomplete\s+series\b)`)

// Extension sets mirror the `extensions:` lists in classifier.core.yml.
// They are duplicated deliberately: basic mode must keep working when
// the classifier workflow is replaced or disabled, and the lists change
// rarely. iso/bin are excluded from software for the reason recorded in
// classifier.core.yml (both are also video disc images).
var basicExtensions = map[model.ContentType][]string{
	model.ContentTypeMusic:     {"mp3", "wav", "flac", "aac", "ogg", "m4a", "mid", "dsf", "ape"},
	model.ContentTypeAudiobook: {"m4b", "aax"},
	model.ContentTypeEbook:     {"azw", "azw3", "azw4", "azw8", "chm", "doc", "docx", "djvu", "epub", "lit", "mobi", "odt", "pdf", "rtf"},
	model.ContentTypeComic:     {"cb7", "cba", "cbr", "cbt", "cbz"},
	model.ContentTypeSoftware:  {"apk", "app", "bat", "deb", "dll", "dmg", "exe", "jar", "lua", "msi", "package", "pkg"},
}

var basicVideoExtensions = []string{"mp4", "mkv", "avi", "mov", "wmv", "flv", "m4v", "mpg", "mpeg", "ts", "vob", "webm"}

func extensionOf(path string) string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
}

// basicResult stamps a coarse content type by byte-majority of file
// extensions, the same voting rule the core CEL workflow uses
// (`sum(ext in set ? size : -size) > 0`). A hint always wins. When no
// bucket holds a majority of bytes the type stays unknown — basic mode
// never guesses beyond the majority rule and never deletes.
func basicResult(t model.Torrent) classification.Result {
	cl := rawResult(t)
	if cl.ContentType.Valid {
		return cl
	}

	type fileFact struct {
		ext  string
		size int64
	}
	files := make([]fileFact, 0, len(t.Files))
	var total int64
	for _, f := range t.Files {
		files = append(files, fileFact{ext: extensionOf(f.Path), size: int64(f.Size)})
		total += int64(f.Size)
	}
	if len(files) == 0 && t.Size > 0 {
		// Single-file torrent whose file inventory has not been stored:
		// the torrent name carries the only extension available.
		files = append(files, fileFact{ext: extensionOf(t.Name), size: int64(t.Size)})
		total = int64(t.Size)
	}
	if total <= 0 {
		return cl
	}

	majority := func(exts []string) bool {
		set := make(map[string]struct{}, len(exts))
		for _, e := range exts {
			set[e] = struct{}{}
		}
		var sum int64
		for _, f := range files {
			if _, ok := set[f.ext]; ok {
				sum += f.size
			}
		}
		return sum*2 > total
	}

	if majority(basicVideoExtensions) {
		kind := model.ContentTypeMovie
		if episodePattern.MatchString(t.Name) {
			kind = model.ContentTypeTvShow
		}
		cl.ContentType = model.NewNullContentType(kind)
		return cl
	}
	// Deterministic order: audiobooks share the audio family with music,
	// so the narrower bucket is tested first.
	for _, kind := range []model.ContentType{
		model.ContentTypeAudiobook,
		model.ContentTypeMusic,
		model.ContentTypeComic,
		model.ContentTypeEbook,
		model.ContentTypeSoftware,
	} {
		if majority(basicExtensions[kind]) {
			cl.ContentType = model.NewNullContentType(kind)
			return cl
		}
	}
	return cl
}
