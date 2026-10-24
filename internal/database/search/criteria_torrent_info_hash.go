package search

import (
	"fmt"
	"strings"

	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
)

func TorrentInfoHashCriteria(infoHashes ...protocol.InfoHash) query.Criteria {
	return infoHashCriteria(model.TableNameTorrent, infoHashes...)
}

func infoHashCriteria(table string, infoHashes ...protocol.InfoHash) query.Criteria {
	if len(infoHashes) == 0 {
		return query.DBCriteria{
			SQL: "FALSE",
		}
	}

	decodes := make([]string, len(infoHashes))
	for i, infoHash := range infoHashes {
		decodes[i] = fmt.Sprintf("DECODE('%s', 'hex')", infoHash.String())
	}

	return query.DBCriteria{
		SQL: fmt.Sprintf("(%s.info_hash IN (%s) OR %s.info_hash IN (SELECT info_hash FROM torrents WHERE info_hash_v2 IN (%s)))", table, strings.Join(decodes, ", "), table, strings.Join(decodes, ", ")),
	}
}

// LegacyTorrentIdentityCriteria keeps Hash20 and v1 HTTP representations from
// advertising a v2 discovery prefix as a SHA-1 identity.
func LegacyTorrentIdentityCriteria(table string) query.Criteria {
	switch table {
	case "torrents", "torrent_contents", "torrent_files":
	default:
		panic("invalid legacy identity table")
	}
	return query.DBCriteria{SQL: "octet_length(" + table + ".info_hash)=20"}
}
