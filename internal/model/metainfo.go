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

// TorrentFromMetadata projects already validated metadata into the catalogue.
// Hashes always come from the validation result, never a discovery prefix or
// caller-supplied name. Padding files are excluded from the v2 file listing.
func TorrentFromMetadata(metadata metainfo.ValidatedInfo, source string, savePieces bool, fileLimit uint) Torrent {
	info := metadata.Info
	hash := metadata.Identities.Canonical()
	t := Torrent{InfoHash: hash, InfoHashV2: metadata.Identities.V2, Name: info.BestName(),
		Size: uint(info.TotalLength()), FilesStatus: FilesStatusMulti, FilesCount: NewNullUint(uint(len(metadata.Files)))}
	if info.Private != nil {
		t.Private = *info.Private
	}
	// In v2 the advisory name does not determine the actual single-file path.
	// Preserve a different file-tree path by using the ordinary file listing.
	if len(metadata.Files) == 1 && (info.MetaVersion == 0 && len(info.Files) == 0 ||
		info.MetaVersion == 2 && len(metadata.Files[0].Path) == 1 && metadata.Files[0].Path[0] == info.BestName()) {
		t.FilesStatus = FilesStatusSingle
		t.FilesCount = NullUint{}
	} else {
		for idx, f := range metadata.Files {
			if uint(idx) >= fileLimit {
				t.FilesStatus = FilesStatusOverThreshold
				break
			}
			t.Files = append(t.Files, TorrentFile{InfoHash: hash, Index: uint(idx), Path: strings.Join(f.BestPath(), "/"), Size: uint(f.Length)})
		}
	}
	if savePieces && len(info.Pieces) > 0 {
		t.Pieces = TorrentPieces{InfoHash: hash, PieceLength: info.PieceLength, Pieces: info.Pieces}
	}
	t.Sources = []TorrentsTorrentSource{{InfoHash: hash, Source: source}}
	return t
}
