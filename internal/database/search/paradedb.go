package search

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/telemetry/dualemit"
	"gorm.io/gorm"
)

const DefaultUnlinkedFallbackTimeout = 40 * time.Millisecond

var (
	unlinkedTimeoutCounter = dualemit.NewCounter(prometheus.CounterOpts{
		Namespace: "dh_waypoint",
		Subsystem: "search",
		Name:      "unlinked_fallback_timeouts_total",
		Help:      "Identifier searches whose optional unlinked FTS fallback exceeded its deadline and returned linked rows only.",
	})

	capabilityCache sync.Map // *sql.DB -> dbCapabilities
)

type dbCapabilities struct {
	paradeDBBM25   bool
	episodeKeysIdx bool
}

// ResolveUnlinkedFallbackTimeout reads SEARCH_UNLINKED_FALLBACK_TIMEOUT or
// returns the 40ms default. Setting 0s disables the sub-deadline.
func ResolveUnlinkedFallbackTimeout() time.Duration {
	raw := os.Getenv("SEARCH_UNLINKED_FALLBACK_TIMEOUT")
	if raw == "" {
		return DefaultUnlinkedFallbackTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return DefaultUnlinkedFallbackTimeout
	}
	return d
}

// DetectDBCapabilities probes optional PostgreSQL extensions and concurrent
// indexes once per underlying *sql.DB handle. Stock postgres:16/18 works with
// zero extensions beyond pg_trgm/btree_gin, while opt-in paradedb/paradedb PG18
// deployments automatically unlock BM25 scoring when torrent_contents_bm25_idx
// is present.
func DetectDBCapabilities(ctx context.Context, db *gorm.DB) (paradeBM25 bool, episodeKeysIdx bool) {
	if db == nil {
		return false, false
	}
	sqlDB, err := db.DB()
	if err != nil || sqlDB == nil {
		return false, false
	}
	if cached, ok := capabilityCache.Load(sqlDB); ok {
		caps := cached.(dbCapabilities)
		return caps.paradeDBBM25, caps.episodeKeysIdx
	}
	var hasParade, hasEpIdx bool
	_ = db.WithContext(ctx).Raw(`SELECT
		EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'pg_search')
		AND to_regclass('torrent_contents_bm25_idx') IS NOT NULL,
		to_regclass('torrent_contents_episode_keys_idx') IS NOT NULL`).Row().Scan(&hasParade, &hasEpIdx)
	caps := dbCapabilities{paradeDBBM25: hasParade, episodeKeysIdx: hasEpIdx}
	capabilityCache.Store(sqlDB, caps)
	return caps.paradeDBBM25, caps.episodeKeysIdx
}

// HasParadeDBBM25 reports whether the connected PostgreSQL instance has both
// the ParadeDB pg_search extension and the torrent_contents_bm25_idx index.
func HasParadeDBBM25(ctx context.Context, db *gorm.DB) bool {
	bm25, _ := DetectDBCapabilities(ctx, db)
	return bm25
}

// TorrentContentEpisodeKeysCriteria builds a GIN array-overlap/containment
// predicate matching torrent_contents_episode_keys_idx for resolved canonical
// TV keys and requested season/episode numbers.
func TorrentContentEpisodeKeysCriteria(keys []model.ContentRef, episodes model.Episodes) query.Criteria {
	if len(keys) == 0 || len(episodes) == 0 {
		return query.And()
	}
	var perSeason []query.Criteria
	for _, entry := range episodes.SeasonEntries() {
		if len(entry.Episodes) == 0 {
			arr := make([]string, 0, len(keys))
			for _, k := range keys {
				arr = append(arr, fmt.Sprintf("%s:%s:%s:s:%d", k.Type, k.Source, k.ID, entry.Season))
			}
			perSeason = append(perSeason, query.DBCriteria{
				SQL:  "torrent_contents_episode_keys(torrent_contents.content_type, torrent_contents.content_source, torrent_contents.content_id, torrent_contents.episodes) && ?::text[]",
				Args: []interface{}{arr},
			})
			continue
		}
		var orPerKey []query.Criteria
		for _, k := range keys {
			wholeKey := fmt.Sprintf("%s:%s:%s:s_all:%d", k.Type, k.Source, k.ID, entry.Season)
			epArr := make([]string, 0, len(entry.Episodes))
			for _, ep := range entry.Episodes {
				epArr = append(epArr, fmt.Sprintf("%s:%s:%s:e:%d:%d", k.Type, k.Source, k.ID, entry.Season, ep))
			}
			orPerKey = append(orPerKey, query.DBCriteria{
				SQL: "(torrent_contents_episode_keys(torrent_contents.content_type, torrent_contents.content_source, torrent_contents.content_id, torrent_contents.episodes) @> ?::text[] " +
					"OR torrent_contents_episode_keys(torrent_contents.content_type, torrent_contents.content_source, torrent_contents.content_id, torrent_contents.episodes) @> ?::text[])",
				Args: []interface{}{[]string{wholeKey}, epArr},
			})
		}
		perSeason = append(perSeason, query.Or(orPerKey...))
	}
	return query.And(perSeason...)
}
