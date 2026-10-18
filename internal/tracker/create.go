package tracker

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/anacrolix/torrent/bencode"
	mi "github.com/anacrolix/torrent/metainfo"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
)

// CreateOptions control .torrent file creation.
type CreateOptions struct {
	// Trackers become the announce URL (first) and announce-list.
	Trackers []string
	// Private sets the BEP-27 private flag inside the info
	// dictionary. Changing it later changes the info hash.
	Private bool
	// Source is the BEP-27 companion field private trackers use to
	// differentiate the same content across sources. Optional.
	Source string
	// PieceLength in bytes; 0 picks by total size.
	PieceLength int64
	Comment     string
}

// DefaultPieceLength picks a piece size targeting roughly
// one-to-two thousand pieces, the range mainstream clients use.
func DefaultPieceLength(total int64) int64 {
	switch {
	case total <= 64<<20:
		return 256 << 10
	case total <= 512<<20:
		return 1 << 20
	case total <= 4<<30:
		return 4 << 20
	default:
		return 16 << 20
	}
}

func totalSize(root string) (int64, error) {
	info, err := os.Stat(root)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return info.Size(), nil
	}
	var total int64
	err = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			total += fi.Size()
		}
		return nil
	})
	return total, err
}

// CreateTorrentFile hashes a file or directory tree into .torrent
// bytes, then re-parses its own output through the hardened
// metainfo validator so a file this function produces is always
// one the core accepts back on upload.
func CreateTorrentFile(root string, opts CreateOptions) ([]byte, protocol.ID, error) {
	total, err := totalSize(root)
	if err != nil {
		return nil, protocol.ID{}, err
	}
	if total <= 0 {
		return nil, protocol.ID{}, errors.New("nothing to hash: empty file or directory")
	}
	pieceLength := opts.PieceLength
	if pieceLength == 0 {
		pieceLength = DefaultPieceLength(total)
	}
	if pieceLength < metainfo.MinPieceLen || pieceLength > metainfo.MaxPieceLen {
		return nil, protocol.ID{}, fmt.Errorf("piece length %d out of bounds", pieceLength)
	}

	info := mi.Info{PieceLength: pieceLength, Source: opts.Source}
	if opts.Private {
		priv := true
		info.Private = &priv
	}
	if err := info.BuildFromFilePath(root); err != nil {
		return nil, protocol.ID{}, fmt.Errorf("hash %s: %w", root, err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		return nil, protocol.ID{}, err
	}
	hash := protocol.ID(mi.HashBytes(infoBytes))
	if _, err := metainfo.ParseMetaInfoBytes(hash, infoBytes); err != nil {
		return nil, protocol.ID{}, fmt.Errorf("created metainfo fails core validation: %w", err)
	}

	meta := mi.MetaInfo{InfoBytes: infoBytes, Comment: opts.Comment, CreatedBy: "dh-waypoint"}
	if len(opts.Trackers) > 0 {
		meta.Announce = opts.Trackers[0]
		tiers := make(mi.AnnounceList, len(opts.Trackers))
		for i, t := range opts.Trackers {
			tiers[i] = []string{t}
		}
		meta.AnnounceList = tiers
	}
	var buf bytes.Buffer
	if err := meta.Write(&buf); err != nil {
		return nil, protocol.ID{}, err
	}
	return buf.Bytes(), hash, nil
}
