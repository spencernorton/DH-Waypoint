package search

import (
	"context"
	"database/sql"

	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"gorm.io/gen"
	"gorm.io/gen/field"
	"gorm.io/gorm"
)

type TorrentContentResultItem struct {
	query.ResultItem
	model.TorrentContent
}

type TorrentContentResult = query.GenericResult[TorrentContentResultItem]

type TorrentContentSearch interface {
	TorrentContent(ctx context.Context, options ...query.Option) (TorrentContentResult, error)
}

// IdentifierTorrentContentSearch is an optional optimized consumer search.
// Other Search implementations retain the ordinary identifier criterion.
type IdentifierTorrentContentSearch interface {
	TorrentContentByIdentifiers(context.Context, []model.ContentRef, ...query.Option) (TorrentContentResult, error)
}

func (s search) TorrentContentByIdentifiers(ctx context.Context, refs []model.ContentRef, options ...query.Option) (result TorrentContentResult, err error) {
	options = append(append([]query.Option(nil), options...), query.Where(torrentContentResolvedIdentifiersCriteria(ctx, refs)))
	if len(refs) == 0 || len(refs) == 1 && refs[0].Source == model.SourceTmdb {
		return s.TorrentContent(ctx, options...)
	}
	// The lookup and page share one read snapshot under the caller's context.
	// Concurrent alias changes
	// cannot turn a pre-resolved key into an omitted or misidentified result.
	db := s.q.TorrentContent.WithContext(ctx).UnderlyingDB().Session(&gorm.Session{NewDB: true})
	err = db.Transaction(func(tx *gorm.DB) error {
		snapshot := s
		snapshot.q = dao.Use(tx)
		var queryErr error
		result, queryErr = snapshot.torrentContent(ctx, true, options...)
		return queryErr
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return result, err
}

func (s search) TorrentContent(ctx context.Context, options ...query.Option) (TorrentContentResult, error) {
	return s.torrentContent(ctx, false, options...)
}

func (s search) torrentContent(ctx context.Context, serial bool, options ...query.Option) (TorrentContentResult, error) {
	options = append([]query.Option{query.SelectAll()}, options...)
	if s.adultPolicy != nil && s.adultPolicy.ExcludesAdult() {
		options = append(options, query.WithFacetExcludedValues(TorrentContentTypeFacetKey, "xxx"))
	}
	execute := query.GenericQuery[TorrentContentResultItem]
	if serial {
		execute = query.GenericQuerySerial[TorrentContentResultItem]
	}
	return execute(
		ctx,
		s.q,
		query.Options(options...),
		model.TableNameTorrentContent,
		func(ctx context.Context, q *dao.Query) query.SubQuery {
			base := q.TorrentContent.WithContext(ctx).ReadDB()
			if s.adultPolicy != nil {
				predicate := s.adultPolicy.TorrentCondition(model.TableNameTorrentContent)
				base = base.Scopes(func(d gen.Dao) gen.Dao {
					do := d.(*gen.DO)
					do.ReplaceDB(do.UnderlyingDB().Where(predicate))
					return d
				})
			}
			return query.GenericSubQuery[dao.ITorrentContentDo]{
				SubQuery: base,
			}
		},
	)
}

func TorrentContentDefaultOption() query.Option {
	return query.Options(
		query.DefaultOption(),
		TorrentContentDefaultHydrate(),
		TorrentContentCoreJoins(),
		query.OrderBy(TorrentContentOrderByPublishedAt.Clauses(OrderDirectionDescending)...),
	)
}

func TorrentContentCoreJoins() query.Option {
	return query.Options(
		query.Join(func(q *dao.Query) []query.TableJoin {
			return []query.TableJoin{
				{
					Table: q.Torrent,
					On: []field.Expr{
						q.TorrentContent.InfoHash.EqCol(q.Torrent.InfoHash),
					},
					Type:      query.TableJoinTypeInner,
					AtMostOne: true, // full torrents primary key
				},
				{
					Table: q.Content,
					On: []field.Expr{
						q.TorrentContent.ContentType.EqCol(q.Content.Type),
						q.TorrentContent.ContentSource.EqCol(q.Content.Source),
						q.TorrentContent.ContentID.EqCol(q.Content.ID),
					},
					Type:      query.TableJoinTypeLeft,
					AtMostOne: true, // full content primary key
				},
			}
		}),
		ContentCoreJoins(),
	)
}

func TorrentContentDefaultHydrate() query.Option {
	return query.Options(
		HydrateTorrentContentTorrent(),
		HydrateTorrentContentContent(),
	)
}

// torrentContentGroupKeySQL is the (content_type, content_source, content_id)
// grouping key — one distinct value per matched title.
const torrentContentGroupKeySQL = model.TableNameTorrentContent + ".content_type, " +
	model.TableNameTorrentContent + ".content_source, " +
	model.TableNameTorrentContent + ".content_id"

// torrentContentGroupInnerOrderSQL selects the representative torrent for each
// title: the group key (leading, as DISTINCT ON requires), then highest seeders
// (NULL treated as -1 so null-seeder torrents sort last, matching the existing
// seeders comparator in order_torrent_content.go), then info_hash as the final
// deterministic tiebreak (unique within a group, so the representative is a pure
// function of the current snapshot and offset pagination cannot dup/skip a
// title on account of a seeder tie).
const torrentContentGroupInnerOrderSQL = torrentContentGroupKeySQL +
	", COALESCE(" + model.TableNameTorrentContent + ".seeders, -1) DESC, " +
	model.TableNameTorrentContent + ".info_hash"

// TorrentContentGroupByContentOption collapses the search to one row per title
// (content), the representative being the highest-seeded torrent. It is
// matched-only: rows with no resolved content (content_id IS NULL) are excluded
// because grouping by a content key is undefined for them — the WHERE below is a
// global scope, so it also constrains the totalCount and the facet aggregations
// to the same matched corpus. Pair with the torrent_contents_content_group_idx
// covering index (migration 00036), whose column order mirrors the inner ORDER
// BY so Postgres feeds the Unique node from an ordered index scan.
func TorrentContentGroupByContentOption() query.Option {
	return query.Options(
		query.GroupByDistinctOn(torrentContentGroupKeySQL, torrentContentGroupInnerOrderSQL),
		query.GroupedCandidatePrefix(),
		query.Where(query.DBCriteria{
			SQL: model.TableNameTorrentContent + ".content_id IS NOT NULL",
		}),
	)
}
