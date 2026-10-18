package search

import (
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/maps"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"gorm.io/gen"
)

func TorrentTagCriteria(tagNames ...string) query.Criteria {
	return query.GenCriteria(func(ctx query.DBContext) (query.Criteria, error) {
		q := ctx.Query()

		return query.OrCriteria{
			Criteria: []query.Criteria{
				query.RawCriteria{
					Query: gen.Exists(
						q.TorrentTag.Where(
							q.TorrentTag.InfoHash.EqCol(q.Torrent.InfoHash),
							q.TorrentTag.Name.In(tagNames...),
						),
					),
					Joins: maps.NewInsertMap(
						maps.MapEntry[string, struct{}]{Key: model.TableNameTorrent},
					),
				},
			},
		}, nil
	})
}
