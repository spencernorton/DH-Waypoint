package search

import (
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/maps"
	"github.com/spencernorton/DH-Waypoint/internal/model"
)

func TorrentFileExtensionCriteria(extensions ...string) query.Criteria {
	return query.GenCriteria(func(query.DBContext) (query.Criteria, error) {
		torrentJoin := maps.NewInsertMap(
			maps.MapEntry[string, struct{}]{Key: model.TableNameTorrent},
		)
		if len(extensions) == 0 {
			return query.RawCriteria{
				Query: "FALSE",
				Joins: torrentJoin,
			}, nil
		}

		return query.OrCriteria{
			Criteria: []query.Criteria{
				query.RawCriteria{
					Query: "torrents.extension IN ?",
					Args:  []interface{}{extensions},
					Joins: torrentJoin,
				},
				query.RawCriteria{
					Query: "EXISTS (SELECT 1 FROM torrent_files_effective WHERE torrent_files_effective.info_hash = torrents.info_hash AND torrent_files_effective.extension IN ?)",
					Args:  []interface{}{extensions},
					Joins: torrentJoin,
				},
			},
		}, nil
	})
}
