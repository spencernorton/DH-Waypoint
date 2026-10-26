package classifier

import (
	"strings"

	"github.com/spencernorton/DH-Waypoint/internal/model"
)

// contradictoryScriptExtensions are executable/script payload extensions
// that model.FileTypeFromExtension does not classify. Extensions already
// classified as software/archive/etc. are handled via FileTypeFromExtension.
var contradictoryScriptExtensions = map[string]bool{
	"bat":      true,
	"cmd":      true,
	"scr":      true,
	"com":      true,
	"ps1":      true,
	"vbs":      true,
	"hta":      true,
	"dll":      true,
	"sys":      true,
	"pkg":      true,
	"appimage": true,
	"lnk":      true,
	"jar":      true,
	"wsf":      true,
}

// executableScriptExtensions is the union used when inspecting individual
// files: it includes extensions already covered by FileTypeFromExtension
// (exe, msi, apk, dmg, ...) so a file check catches them even when the
// file's Path is empty and only Extension is set.
var executableScriptExtensions = map[string]bool{
	"exe":      true,
	"msi":      true,
	"bat":      true,
	"cmd":      true,
	"scr":      true,
	"com":      true,
	"ps1":      true,
	"vbs":      true,
	"hta":      true,
	"dll":      true,
	"sys":      true,
	"apk":      true,
	"dmg":      true,
	"pkg":      true,
	"appimage": true,
	"lnk":      true,
	"jar":      true,
	"wsf":      true,
}

func normalizeExtension(ext string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
}

// isContradictoryNonVideoExtension reports whether a single extension is
// known evidence against video (movie/tv_show/xxx) classification.
// Unknown pseudo-extensions from dot-separated scene release names (web,
// hdtv, bluray, x264, ...) are not valid file types and return false, so
// bare release names with no file info are never blocked by this guard.
func isContradictoryNonVideoExtension(ext string) bool {
	ext = normalizeExtension(ext)
	if ext == "" {
		return false
	}
	if ft := model.FileTypeFromExtension(ext); ft.Valid && (ft.FileType == model.FileTypeSoftware || ft.FileType == model.FileTypeArchive) {
		return true
	}
	return contradictoryScriptExtensions[ext]
}

// HasContradictoryNonVideoFiles is the exported form for other packages
// (e.g. processors) that must apply the same guard.
func HasContradictoryNonVideoFiles(t model.Torrent) bool {
	return hasContradictoryNonVideoFiles(t)
}

// hasContradictoryNonVideoFiles reports whether the torrent's known file
// evidence contradicts video classification.
func hasContradictoryNonVideoFiles(t model.Torrent) bool {
	if len(t.Files) > 0 {
		hasVideo := false
		hasContradictory := false
		for _, f := range t.Files {
			ft := f.FileType()
			if ft.Valid && ft.FileType == model.FileTypeVideo {
				hasVideo = true
			}
			if ft.Valid && (ft.FileType == model.FileTypeSoftware || ft.FileType == model.FileTypeArchive) {
				hasContradictory = true
			}
			ext := ""
			if fe := model.FileExtensionFromPath(f.Path); fe.Valid {
				ext = fe.String
			}
			if ext == "" && f.Extension.Valid {
				ext = normalizeExtension(f.Extension.String)
			}
			if ext != "" && executableScriptExtensions[normalizeExtension(ext)] {
				hasContradictory = true
			}
		}
		if !hasVideo && hasContradictory {
			return true
		}
		return false
	}
	if t.Extension.Valid {
		ext := normalizeExtension(t.Extension.String)
		return isContradictoryNonVideoExtension(ext)
	}
	ext := model.FileExtensionFromPath(t.Name)
	if ext.Valid && isContradictoryNonVideoExtension(ext.String) {
		return true
	}
	return false
}
