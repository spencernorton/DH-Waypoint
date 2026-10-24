package search

import (
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
