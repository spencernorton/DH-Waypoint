package model

import (
	"errors"
	"strings"

	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
)

func (t Torrent) LegacyInfoHash() (protocol.ID, error) {
	if t.InfoHash.IsV2() {
		return protocol.ID{}, errors.New("v2 identity requires the v2 API")
	}
	return t.InfoHash.WireID(), nil
}

func (t TorrentFile) LegacyInfoHash() (protocol.ID, error) {
	if t.InfoHash.IsV2() {
		return protocol.ID{}, errors.New("v2 identity requires the v2 API")
	}
	return t.InfoHash.WireID(), nil
}

// IsPaddingFile identifies BEP 47 v1 padding files (attr containing 'p', a
// ".pad" directory segment, or BitComet padding placeholders) so they do not
// inflate torrent_files storage, files_count, or fileLimit cutoffs.
func IsPaddingFile(attr string, pathParts []string) bool {
	if strings.Contains(attr, "p") {
		return true
	}
	for _, p := range pathParts {
		if p == ".pad" || strings.HasPrefix(p, "_____padding_file_") {
			return true
		}
	}
	return false
}

// TorrentFromMetadata projects already validated metadata into the catalogue.
// Hashes always come from the validation result, never a discovery prefix or
// caller-supplied name. Padding files are excluded from both v1 and v2 listings
// while preserving each real file's original metainfo index.
func TorrentFromMetadata(metadata metainfo.ValidatedInfo, source string, savePieces bool, fileLimit uint) Torrent {
	info := metadata.Info
	hash := metadata.Identities.Canonical()
	type indexedFile struct {
		idx  int
		path string
		size uint
	}
	realFiles := make([]indexedFile, 0, len(metadata.Files))
	var realTotal uint
	seenPaths := make(map[string]struct{}, len(metadata.Files))
	for idx, f := range metadata.Files {
		parts := f.BestPath()
		if IsPaddingFile(f.Attr, parts) {
			continue
		}
		sz := uint(f.Length)
		realTotal += sz
		path := strings.Join(parts, "/")
		// Legacy storage keys files by (info_hash, path) with first-writer-wins;
		// drop later duplicates here so FilesCount matches what is stored.
		if _, dup := seenPaths[path]; dup {
			continue
		}
		seenPaths[path] = struct{}{}
		realFiles = append(realFiles, indexedFile{
			idx:  idx,
			path: path,
			size: sz,
		})
	}

	t := Torrent{
		InfoHash:    hash,
		InfoHashV2:  metadata.Identities.V2,
		Name:        info.BestName(),
		Size:        realTotal,
		FilesStatus: FilesStatusMulti,
		FilesCount:  NewNullUint(uint(len(realFiles))),
	}
	if info.Private != nil {
		t.Private = *info.Private
	}
	// In v2 the advisory name does not determine the actual single-file path.
	// Preserve a different file-tree path by using the ordinary file listing.
	if len(realFiles) == 1 && (info.MetaVersion == 0 && len(info.Files) == 0 ||
		info.MetaVersion == 2 && len(metadata.Files) == 1 && len(metadata.Files[0].Path) == 1 && metadata.Files[0].Path[0] == info.BestName()) {
		t.FilesStatus = FilesStatusSingle
		t.FilesCount = NullUint{}
		t.Size = uint(info.TotalLength())
	} else {
		for _, rf := range realFiles {
			if uint(len(t.Files)) >= fileLimit {
				t.FilesStatus = FilesStatusOverThreshold
				break
			}
			t.Files = append(t.Files, TorrentFile{InfoHash: hash, Index: uint(rf.idx), Path: rf.path, Size: rf.size})
		}
	}
	if savePieces && len(info.Pieces) > 0 {
		t.Pieces = TorrentPieces{InfoHash: hash, PieceLength: info.PieceLength, Pieces: info.Pieces}
	}
	t.Sources = []TorrentsTorrentSource{{InfoHash: hash, Source: source}}
	return t
}
