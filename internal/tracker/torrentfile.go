package tracker

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/anacrolix/torrent/bencode"
	mi "github.com/anacrolix/torrent/metainfo"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
)

// MaxTorrentFileBytes bounds an uploaded .torrent file: the info
// dictionary cap plus room for outer announce metadata.
const MaxTorrentFileBytes = metainfo.MaxMetaInfoBytes + 2*1024*1024

// ParseTorrentFile extracts and validates the info dictionary of an
// uploaded .torrent file, returning its info hash. Validation is the
// same hardening the BEP-9 receive path applies (hash binding, size,
// UTF-8 and path bounds).
func ParseTorrentFile(torrentFile []byte) (protocol.ID, metainfo.Info, error) {
	if len(torrentFile) == 0 || len(torrentFile) > MaxTorrentFileBytes {
		return protocol.ID{}, metainfo.Info{}, errors.New("torrent file size out of bounds")
	}
	var raw struct {
		Info bencode.Bytes `bencode:"info"`
	}
	if err := bencode.Unmarshal(torrentFile, &raw); err != nil {
		return protocol.ID{}, metainfo.Info{}, fmt.Errorf("unmarshal torrent file: %w", err)
	}
	if len(raw.Info) == 0 {
		return protocol.ID{}, metainfo.Info{}, errors.New("torrent file has no info dictionary")
	}
	hash := protocol.ID(mi.HashBytes(raw.Info))
	info, err := metainfo.ParseMetaInfoBytes(hash, raw.Info)
	if err != nil {
		return protocol.ID{}, metainfo.Info{}, err
	}
	return hash, info, nil
}

// RewriteAnnounce returns the torrent file with its announce fields
// replaced by announceURL. Only outer (non-info) keys change: the
// raw info bytes are preserved verbatim, so the info hash is
// unchanged. An empty announceURL returns the input untouched.
func RewriteAnnounce(torrentFile []byte, announceURL string) ([]byte, error) {
	if announceURL == "" {
		return torrentFile, nil
	}
	meta, err := mi.Load(bytes.NewReader(torrentFile))
	if err != nil {
		return nil, fmt.Errorf("load torrent file: %w", err)
	}
	meta.Announce = announceURL
	meta.AnnounceList = mi.AnnounceList{{announceURL}}
	var buf bytes.Buffer
	if err := meta.Write(&buf); err != nil {
		return nil, fmt.Errorf("write torrent file: %w", err)
	}
	return buf.Bytes(), nil
}
