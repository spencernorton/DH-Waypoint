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

	// Build the small set of requested keys and hybrid aliases first. An OR
	// with an alias subquery makes PostgreSQL scan the whole outer table even
	// when the caller is hydrating only a page of results. The semi-join keeps
	// direct keys (including not-yet-catalogued ones) and full v2 aliases while
	// allowing an index lookup for each resulting key.
	keys := strings.Join(decodes, ", ")
	return query.DBCriteria{
		SQL: fmt.Sprintf("%s.info_hash IN (SELECT unnest(ARRAY[%s]) UNION SELECT info_hash FROM torrents WHERE info_hash_v2 IN (%s))", table, keys, keys),
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
