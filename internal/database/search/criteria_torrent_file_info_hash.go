package search

import (
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
)

func TorrentFileInfoHashCriteria(infoHashes ...protocol.InfoHash) query.Criteria {
	return infoHashCriteria(model.TableNameTorrentFile, infoHashes...)
}
