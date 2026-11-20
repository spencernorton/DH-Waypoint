package search

import (
	"context"
	"fmt"
	"strings"

	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
)

// TorrentContentEpisodesCriteria filters torrent_contents by season/episode.
//
// The predicate mirrors model.Episodes.HasEpisode: a stored whole-season pack
// (season key present with an empty object) contains *every* episode of that
// season, so it must satisfy both season-only and specific-episode queries.
//
//   - Season-only request ({season}): match any row carrying that season key —
//     whole-season packs, partial/volume packs, and individual episodes alike.
//     Uses jsonb_exists() rather than the `?` operator because DBCriteria.SQL is
//     raw SQL where GORM treats a literal `?` as a bind placeholder.
//   - Episode request ({season: {episodes}}): match rows whose season is a
//     whole-season pack OR that enumerate all requested episodes. When ParadeDB
//     BM25 with the generated ep_text column is active, emit an equivalent
//     ep_text Tantivy predicate so episode filtering pushes into the BM25 scan.
func TorrentContentEpisodesCriteria(episodes model.Episodes) query.Criteria {
	return query.GenCriteria(func(dbCtx query.DBContext) (query.Criteria, error) {
		useBM25EpText := false
		if dbCtx != nil && ResolveBM25Mode() != BM25ModeOff {
			if bm25Ctx, ok := dbCtx.(interface{ BM25Enabled() bool }); !ok || bm25Ctx.BM25Enabled() {
				db := dbCtx.Query().TorrentContent.UnderlyingDB()
				useBM25EpText = HasParadeDBBM25(context.Background(), db) && HasBM25EpisodeText(context.Background(), db)
			}
		}

		and := make([]query.Criteria, 0, len(episodes))

		for _, s := range episodes.SeasonEntries() {
			if len(s.Episodes) == 0 {
				and = append(and, query.DBCriteria{
					SQL: fmt.Sprintf(
						"jsonb_exists(torrent_contents.episodes, '%d')",
						s.Season,
					),
				})
			} else if useBM25EpText {
				wholeTok := fmt.Sprintf("s%dall", s.Season)
				if len(s.Episodes) == 1 {
					and = append(and, query.DBCriteria{
						SQL:  "torrent_contents.ep_text ||| ?",
						Args: []interface{}{fmt.Sprintf("%s s%de%d", wholeTok, s.Season, s.Episodes[0])},
					})
				} else {
					epToks := make([]string, 0, len(s.Episodes))
					for _, e := range s.Episodes {
						epToks = append(epToks, fmt.Sprintf("s%de%d", s.Season, e))
					}
					and = append(and, query.DBCriteria{
						SQL:  "(torrent_contents.ep_text &&& ? OR torrent_contents.ep_text &&& ?)",
						Args: []interface{}{wholeTok, strings.Join(epToks, " ")},
					})
				}
			} else {
				keyParts := make([]string, 0, len(s.Episodes))
				for _, e := range s.Episodes {
					keyParts = append(keyParts, fmt.Sprintf(`"%d":{}`, e))
				}

				and = append(and, query.DBCriteria{
					SQL: fmt.Sprintf(
						"(torrent_contents.episodes #> '{%d}' = '{}'::jsonb "+
							"OR torrent_contents.episodes #> '{%d}' @> '{%s}'::jsonb)",
						s.Season,
						s.Season,
						strings.Join(keyParts, ","),
					),
				})
			}
		}

		return query.AndCriteria{
			Criteria: and,
		}, nil
	})
}
