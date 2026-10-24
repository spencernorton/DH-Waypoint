package search

import (
	"strings"

	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
)

// TorrentContentIdentityCriteria uses the indexed canonical identity directly.
// It remains an additional predicate of the ordinary serving-policy search.
func TorrentContentIdentityCriteria(ref model.ContentRef) query.Criteria {
	return query.DBCriteria{
		SQL:  "torrent_contents.content_type = ? AND torrent_contents.content_source = ? AND torrent_contents.content_id = ?",
		Args: []interface{}{ref.Type.String(), ref.Source, ref.ID},
	}
}

// TorrentContentIdentifiersCriteria treats ID systems as alternatives. Resolve
// the small identifier key set before probing torrent_contents, rather than
// placing a correlated attribute lookup in an OR for every catalogue row.
// TMDB is the canonical namespace; its single typed fast path reuses the direct
// identity predicate. External attributes must be real IDs, never titles.
func TorrentContentIdentifiersCriteria(refs ...model.ContentRef) query.Criteria {
	if len(refs) == 1 && refs[0].Source == model.SourceTmdb {
		return TorrentContentIdentityCriteria(refs[0])
	}
	if len(refs) == 0 {
		return query.DBCriteria{SQL: "FALSE"}
	}
	var values, alternatives []string
	var args, alternativeArgs []interface{}
	for _, ref := range refs {
		values = append(values, "(?, ?, ?)")
		args = append(args, ref.Type.String(), ref.Source, ref.ID)
		if ref.Source != model.SourceTmdb {
			alternatives = append(alternatives, "(identifier.content_type = ? AND identifier.source = ? AND identifier.value = ?)")
			alternativeArgs = append(alternativeArgs, ref.Type.String(), ref.Source, ref.ID)
		}
	}
	sql := `(torrent_contents.content_type, torrent_contents.content_source, torrent_contents.content_id)
 IN (VALUES ` + strings.Join(values, ", ")
	if len(alternatives) > 0 {
		sql += ` UNION ALL SELECT identifier.content_type, identifier.content_source, identifier.content_id
 FROM content_attributes identifier WHERE identifier.key = 'id' AND (` + strings.Join(alternatives, " OR ") + ")"
		args = append(args, alternativeArgs...)
	}
	return query.DBCriteria{SQL: sql + ")", Args: args}
}
