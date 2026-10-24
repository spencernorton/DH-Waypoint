package search

import (
	"context"
	"strings"
	"time"

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
	keys, args := torrentContentIdentifierKeyQuery(refs, false)
	return query.DBCriteria{
		SQL:  "(torrent_contents.content_type, torrent_contents.content_source, torrent_contents.content_id) IN (" + keys + ")",
		Args: args,
	}
}

func torrentContentIdentifierKeyQuery(refs []model.ContentRef, existingCanonical bool) (string, []interface{}) {
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
	sql := "VALUES " + strings.Join(values, ", ")
	if existingCanonical {
		sql = `SELECT canonical.type AS content_type, canonical.source AS content_source, canonical.id AS content_id
 FROM content canonical WHERE (canonical.type, canonical.source, canonical.id) IN (` + sql + ")"
	}
	if len(alternatives) > 0 {
		sql += ` UNION ALL SELECT identifier.content_type, identifier.content_source, identifier.content_id
 FROM content_attributes identifier WHERE identifier.key = 'id' AND (` + strings.Join(alternatives, " OR ") + ")"
		args = append(args, alternativeArgs...)
	}
	return sql, args
}

// Resolve inside the read snapshot used by TorrentContentByIdentifiers. A
// single canonical key can use the ordered identity index even with a generic
// prepared plan. Ambiguous/multiple keys keep the complete semijoin semantics.
func torrentContentResolvedIdentifiersCriteria(requestCtx context.Context, refs []model.ContentRef) query.Criteria {
	refs = append([]model.ContentRef(nil), refs...)
	guard := TorrentContentIdentifiersCriteria(refs...)
	if len(refs) == 0 || len(refs) == 1 && refs[0].Source == model.SourceTmdb {
		return guard
	}
	keysSQL, args := torrentContentIdentifierKeyQuery(refs, true)
	lookupSQL := "SELECT DISTINCT content_type, content_source, content_id FROM (" + keysSQL + ") identifier_keys LIMIT 2"
	return query.GenCriteria(func(dbCtx query.DBContext) (query.Criteria, error) {
		lookupCtx, cancel := context.WithTimeout(requestCtx, 5*time.Second)
		defer cancel()
		if err := lookupCtx.Err(); err != nil {
			return nil, err
		}
		var keys []struct {
			ContentType   string
			ContentSource string
			ContentID     string
		}
		db := dbCtx.Query().Content.UnderlyingDB().WithContext(lookupCtx)
		if db.DryRun {
			return guard, nil
		}
		if err := db.Raw(lookupSQL, args...).Scan(&keys).Error; err != nil {
			return nil, err
		}
		if len(keys) != 1 {
			return guard, nil
		}
		for _, ref := range refs {
			if ref.Type.String() == keys[0].ContentType {
				canonical := model.ContentRef{Type: ref.Type, Source: keys[0].ContentSource, ID: keys[0].ContentID}
				// Keep the authoritative identifier predicate as well as the
				// selective key. A frozen predicate cannot elevate a revoked alias.
				return query.And(TorrentContentIdentityCriteria(canonical), guard), nil
			}
		}
		return guard, nil
	})
}
