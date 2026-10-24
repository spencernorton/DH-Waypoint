package importer

import (
	"errors"

	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
)

// ItemFromTorrentBytes imports complete metainfo, including v2 outer layers.
// All identity, name, size and privacy facts come from the verified bytes.
func ItemFromTorrentBytes(raw []byte, expected *protocol.InfoHash, source string) (Item, error) {
	parsed, err := metainfo.ParseTorrentBytes(raw, expected)
	if err != nil {
		return Item{}, err
	}
	return itemFromValidated(parsed, source), nil
}

// ItemFromMetadata indexes info-only peer metadata without claiming to have
// validated unavailable outer piece layers or downloaded any payload.
func ItemFromMetadata(parsed metainfo.ValidatedInfo, source string) (Item, error) {
	// Do not accept a caller-constructed validation result as authority.
	rechecked, err := metainfo.ParseInfoBytes(parsed.Identities.Canonical(), parsed.InfoBytes)
	if err != nil {
		return Item{}, err
	}
	if !sameIdentities(rechecked.Identities, parsed.Identities) {
		return Item{}, errors.New("metadata identity mismatch")
	}
	return itemFromValidated(rechecked, source), nil
}

func sameIdentities(a, b protocol.TorrentIdentities) bool {
	return a.Canonical() == b.Canonical() && (a.V1 == nil) == (b.V1 == nil) && (a.V2 == nil) == (b.V2 == nil) &&
		(a.V2 == nil || *a.V2 == *b.V2)
}

func itemFromValidated(parsed metainfo.ValidatedInfo, source string) Item {
	private := parsed.Info.Private != nil && *parsed.Info.Private
	return Item{Source: source, InfoHash: parsed.Identities.Canonical(), Name: parsed.Info.BestName(), Size: uint(parsed.Info.TotalLength()), Private: private, metadata: &parsed}
}
