package search

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
)

// maxResolvedIdentifierKeys bounds the equality arms in a page predicate built
// from materialised identifier keys. Request refs are few (the Torznab
// adapter emits at most five), and each external ID is attributed to a
// handful of canonical contents. Beyond this bound the complete
// authoritative predicate is kept instead of a truncated key set.
const maxResolvedIdentifierKeys = 32

// dedupeContentRefs removes exact duplicate (type, source, id) refs while
// preserving first-seen order, so duplicate inputs cannot inflate the VALUES
// and alias branches (and bound parameter counts) of identifier predicates.
func dedupeContentRefs(refs []model.ContentRef) []model.ContentRef {
	seen := make(map[model.ContentRef]struct{}, len(refs))
	out := make([]model.ContentRef, 0, len(refs))
	for _, ref := range refs {
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	return out
}

// TorrentContentIdentityCriteria uses the indexed canonical identity directly.
// It remains an additional predicate of the ordinary serving-policy search.
func TorrentContentIdentityCriteria(ref model.ContentRef) query.Criteria {
	return query.DBCriteria{
		SQL:  "torrent_contents.content_type = ? AND torrent_contents.content_source = ? AND torrent_contents.content_id = ?",
		Args: []interface{}{ref.Type.String(), ref.Source, ref.ID},
	}
}

// torrentContentKeysCriteria matches any of a small set of concrete canonical
// keys with one equality arm per key. Every arm fixes the full leading prefix
// of torrent_contents_identity_published_page_idx, so the planner can probe
// the index per key (BitmapOr for several keys) under custom and generic
// prepared plans alike. Keys are de-duplicated and sorted so the generated
// SQL and parameter order are deterministic for a given key set.
func torrentContentKeysCriteria(keys []model.ContentRef) query.Criteria {
	keys = dedupeContentRefs(keys)
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Type != keys[j].Type {
			return keys[i].Type < keys[j].Type
		}
		if keys[i].Source != keys[j].Source {
			return keys[i].Source < keys[j].Source
		}
		return keys[i].ID < keys[j].ID
	})
	if len(keys) == 1 {
		return TorrentContentIdentityCriteria(keys[0])
	}
	arms := make([]string, 0, len(keys))
	args := make([]interface{}, 0, len(keys)*3)
	for _, key := range keys {
		arms = append(arms, "(torrent_contents.content_type = ? AND torrent_contents.content_source = ? AND torrent_contents.content_id = ?)")
		args = append(args, key.Type.String(), key.Source, key.ID)
	}
	return query.DBCriteria{SQL: "(" + strings.Join(arms, " OR ") + ")", Args: args}
}

// TorrentContentIdentifiersCriteria treats ID systems as alternatives. Resolve
// the small identifier key set before probing torrent_contents, rather than
// placing a correlated attribute lookup in an OR for every catalogue row.
// TMDB is the canonical namespace; its single typed fast path reuses the direct
// identity predicate. External attributes must be real IDs, never titles.
//
// This predicate embeds the key set as a subquery semijoin. On a large
// catalogue that shape is planner-fragile — and once a prepared statement
// switches to a generic plan the planner can no longer see that the key set
// is tiny. Snapshot callers should prefer TorrentContentByIdentifiers, which
// materialises the same key set into equality arms; this predicate remains
// the complete fallback for ordinary (non-snapshot) searches.
func TorrentContentIdentifiersCriteria(refs ...model.ContentRef) query.Criteria {
	refs = dedupeContentRefs(refs)
	if len(refs) == 1 && refs[0].Source == model.SourceTmdb {
		return TorrentContentIdentityCriteria(refs[0])
	}
	if len(refs) == 0 {
		return query.DBCriteria{SQL: "FALSE"}
	}
	keys, args := torrentContentIdentifierKeyQuery(refs)
	return query.DBCriteria{
		SQL:  "(torrent_contents.content_type, torrent_contents.content_source, torrent_contents.content_id) IN (" + keys + ")",
		Args: args,
	}
}

// torrentContentIdentifierKeyQuery returns the complete key set an identifier
// request can match: every requested (type, source, id) tuple itself —
// torrent_contents may carry that tuple as its canonical identity — plus,
// for non-TMDB sources, the canonical contents the ID is attributed to in
// content_attributes. The first branch names its columns so the query can
// also be materialised directly by the snapshot lookup below.
func torrentContentIdentifierKeyQuery(refs []model.ContentRef) (string, []interface{}) {
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
	sql := "SELECT requested.content_type, requested.content_source, requested.content_id" +
		" FROM (VALUES " + strings.Join(values, ", ") + ") AS requested(content_type, content_source, content_id)"
	if len(alternatives) > 0 {
		sql += ` UNION ALL SELECT identifier.content_type, identifier.content_source, identifier.content_id
 FROM content_attributes identifier WHERE identifier.key = 'id' AND (` + strings.Join(alternatives, " OR ") + ")"
		args = append(args, alternativeArgs...)
	}
	return sql, args
}

type resolvedIdentifierKey struct {
	ContentType   string `gorm:"column:content_type"`
	ContentSource string `gorm:"column:content_source"`
	ContentID     string `gorm:"column:content_id"`
}

// Resolve inside the read snapshot used by TorrentContentByIdentifiers.
// Materialising the distinct key set with the same key query that backs
// TorrentContentIdentifiersCriteria, then probing torrent_contents with one
// equality arm per key, is exactly equivalent to the authoritative predicate
// within that snapshot — including requested tuples that are themselves
// canonical identities and IDs attributed to several contents — so the page
// query must not also keep the subquery semijoin. Keeping it (previously
// ANDed alongside a singleton key, or used alone whenever resolution found
// more than one key) lets the planner drive the page from the semijoin
// instead of the identity index: the dual-ID planner cliff. A revoked alias
// cannot leak either way, because lookup and page read one repeatable-read
// snapshot and the next request resolves afresh.
func torrentContentResolvedIdentifiersCriteria(requestCtx context.Context, refs []model.ContentRef) query.Criteria {
	refs = dedupeContentRefs(append([]model.ContentRef(nil), refs...))
	guard := TorrentContentIdentifiersCriteria(refs...)
	if len(refs) == 0 || len(refs) == 1 && refs[0].Source == model.SourceTmdb {
		return guard
	}
	keysSQL, args := torrentContentIdentifierKeyQuery(refs)
	lookupSQL := "SELECT DISTINCT content_type, content_source, content_id FROM (" + keysSQL + ") identifier_keys LIMIT " +
		strconv.Itoa(maxResolvedIdentifierKeys+1)
	return query.GenCriteria(func(dbCtx query.DBContext) (query.Criteria, error) {
		lookupCtx, cancel := context.WithTimeout(requestCtx, 5*time.Second)
		defer cancel()
		if err := lookupCtx.Err(); err != nil {
			return nil, err
		}
		db := dbCtx.Query().Content.UnderlyingDB().WithContext(lookupCtx)
		if db.DryRun {
			return guard, nil
		}
		var rows []resolvedIdentifierKey
		if err := db.Raw(lookupSQL, args...).Scan(&rows).Error; err != nil {
			return nil, err
		}
		if len(rows) == 0 || len(rows) > maxResolvedIdentifierKeys {
			return guard, nil
		}
		keys := make([]model.ContentRef, 0, len(rows))
		for _, row := range rows {
			keys = append(keys, model.ContentRef{
				Type:   model.ContentType(row.ContentType),
				Source: row.ContentSource,
				ID:     row.ContentID,
			})
		}
		return torrentContentKeysCriteria(keys), nil
	})
}
