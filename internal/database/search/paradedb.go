package search

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/telemetry/dualemit"
	"gorm.io/gorm"
)

const DefaultUnlinkedFallbackTimeout = 40 * time.Millisecond

type episodesHintContextKey struct{}

// WithEpisodesHint attaches requested TV season/episode filters to ctx so
// identifier-resolved queries can automatically apply the composite
// torrent_contents_episode_keys_idx GIN predicate when present.
func WithEpisodesHint(ctx context.Context, episodes model.Episodes) context.Context {
	if len(episodes) == 0 {
		return ctx
	}
	return context.WithValue(ctx, episodesHintContextKey{}, episodes)
}

// EpisodesHintFromContext extracts any TV season/episode hint attached by WithEpisodesHint.
func EpisodesHintFromContext(ctx context.Context) (model.Episodes, bool) {
	if ctx == nil {
		return nil, false
	}
	eps, ok := ctx.Value(episodesHintContextKey{}).(model.Episodes)
	return eps, ok && len(eps) > 0
}

var (
	unlinkedTimeoutCounter = dualemit.NewCounter(prometheus.CounterOpts{
		Namespace: "dh_waypoint",
		Subsystem: "search",
		Name:      "unlinked_fallback_timeouts_total",
		Help:      "Identifier searches whose optional unlinked FTS fallback exceeded its deadline and returned linked rows only.",
	})

	capabilityCache sync.Map // *sql.DB -> dbCapabilities
)

// PrometheusCollectors returns the search-layer Prometheus collectors to
// register with the application metrics registry.
func PrometheusCollectors() []prometheus.Collector {
	return []prometheus.Collector{
		unlinkedTimeoutCounter,
		query.SearchQueriesCounter(),
	}
}

var bm25OptionalPushdownCols = []string{
	"video_codec",
	"video_source",
	"is_anime",
	"content_source",
	"content_id",
}

type dbCapabilities struct {
	paradeDBBM25    bool
	bm25EpisodeText bool
	bm25ExtraCols   []string
	episodeKeysIdx  bool
}

// ResetDBCapabilitiesCache clears cached extension/index detection so tests
// creating indexes dynamically on an existing connection can re-probe cleanly.
func ResetDBCapabilitiesCache() {
	capabilityCache.Clear()
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

// BM25Mode controls whether eligible torrent_contents text searches may use
// the opt-in ParadeDB BM25 execution path.
type BM25Mode string

const (
	// BM25ModeAuto uses BM25 whenever the ParadeDB capability probe passes.
	BM25ModeAuto BM25Mode = "auto"
	// BM25ModeOn is an explicit opt-in with the same detection requirement.
	BM25ModeOn BM25Mode = "on"
	// BM25ModeOff always keeps the stock tsquery execution path.
	BM25ModeOff BM25Mode = "off"
)

// ResolveBM25Mode reads SEARCH_BM25_MODE (auto default, on, off). Unknown
// values fall back to auto.
func ResolveBM25Mode() BM25Mode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SEARCH_BM25_MODE"))) {
	case string(BM25ModeOn):
		return BM25ModeOn
	case string(BM25ModeOff):
		return BM25ModeOff
	default:
		return BM25ModeAuto
	}
}

func probeDBCapabilities(ctx context.Context, cp gorm.ConnPool) (dbCapabilities, error) {
	var hasParade, hasEpText, hasEpIdx bool
	var indexDef string
	err := cp.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'pg_search')
		AND EXISTS(SELECT 1 FROM pg_index WHERE indexrelid = to_regclass('torrent_contents_bm25_idx') AND indisvalid AND indisready)
		AND EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('torrent_contents') AND attname = 'search_text' AND NOT attisdropped),
		EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid = to_regclass('torrent_contents') AND attname = 'ep_text' AND NOT attisdropped),
		to_regclass('torrent_contents_episode_keys_idx') IS NOT NULL,
		COALESCE(pg_get_indexdef(to_regclass('torrent_contents_bm25_idx')), '')`).Scan(&hasParade, &hasEpText, &hasEpIdx, &indexDef)
	var extraCols []string
	if hasParade && indexDef != "" {
		lowerDef := strings.ToLower(indexDef)
		for _, col := range bm25OptionalPushdownCols {
			if strings.Contains(lowerDef, col) {
				extraCols = append(extraCols, col)
			}
		}
	}
	return dbCapabilities{paradeDBBM25: hasParade, bm25EpisodeText: hasEpText, bm25ExtraCols: extraCols, episodeKeysIdx: hasEpIdx}, err
}

func loadDBCapabilities(ctx context.Context, db *gorm.DB) dbCapabilities {
	if db == nil || db.DryRun {
		return dbCapabilities{}
	}
	sqlDB, err := db.DB()
	if err != nil || sqlDB == nil {
		return dbCapabilities{}
	}
	if cached, ok := capabilityCache.Load(sqlDB); ok {
		return cached.(dbCapabilities)
	}
	// Query through the active ConnPool (preserving any caller transaction on a
	// single-connection pool) without invoking GORM Row callbacks.
	cp := db.ConnPool
	if db.Statement != nil && db.Statement.ConnPool != nil {
		cp = db.Statement.ConnPool
	}
	if cp == nil {
		return dbCapabilities{}
	}
	caps, err := probeDBCapabilities(ctx, cp)
	if err == nil {
		capabilityCache.Store(sqlDB, caps)
	}
	return caps
}

// DetectDBCapabilities probes optional PostgreSQL extensions and concurrent
// indexes once per underlying *sql.DB handle. Stock postgres:16/18 works with
// zero extensions beyond pg_trgm/btree_gin, while opt-in paradedb/paradedb PG18
// deployments automatically unlock BM25 scoring when a valid
// torrent_contents_bm25_idx and the generated search_text column are present.
func DetectDBCapabilities(ctx context.Context, db *gorm.DB) (paradeBM25 bool, episodeKeysIdx bool) {
	caps := loadDBCapabilities(ctx, db)
	return caps.paradeDBBM25, caps.episodeKeysIdx
}

// HasParadeDBBM25 reports whether the connected PostgreSQL instance has the
// ParadeDB pg_search extension, a valid torrent_contents_bm25_idx index, and
// the generated search_text column.
func HasParadeDBBM25(ctx context.Context, db *gorm.DB) bool {
	bm25, _ := DetectDBCapabilities(ctx, db)
	return bm25
}

// HasBM25EpisodeText reports whether the generated ep_text column probed
// alongside search_text is present on torrent_contents.
func HasBM25EpisodeText(ctx context.Context, db *gorm.DB) bool {
	return loadDBCapabilities(ctx, db).bm25EpisodeText
}

// BM25ExtraColumns reports optional filter columns (video_codec, video_source,
// is_anime, content_source, content_id) confirmed indexed in the live
// torrent_contents_bm25_idx definition.
func BM25ExtraColumns(ctx context.Context, db *gorm.DB) []string {
	return loadDBCapabilities(ctx, db).bm25ExtraCols
}

// BM25Status is a fresh (uncached) snapshot of the opt-in ParadeDB BM25
// installation state for operator reporting.
type BM25Status struct {
	Mode                 BM25Mode `json:"mode"`
	ExtensionAvailable   bool     `json:"extension_available"`
	ExtensionInstalled   bool     `json:"extension_installed"`
	ExtensionVersion     string   `json:"extension_version,omitempty"`
	SearchTextColumn     bool     `json:"search_text_column"`
	SearchTextGenerated  bool     `json:"search_text_generated"`
	EpisodeTextColumn    bool     `json:"episode_text_column"`
	EpisodeTextGenerated bool     `json:"episode_text_generated"`
	IndexExists          bool     `json:"index_exists"`
	IndexValid           bool     `json:"index_valid"`
	Active               bool     `json:"active"`
}

// ParadeDBBM25Status probes the current database for the pg_search
// extension, generated columns, and BM25 index without using the capability
// cache, so it reflects DDL applied moments earlier.
func ParadeDBBM25Status(ctx context.Context, db *gorm.DB) (BM25Status, error) {
	status := BM25Status{Mode: ResolveBM25Mode()}
	if db == nil {
		return status, fmt.Errorf("paradedb bm25 status: nil database handle")
	}
	if err := db.WithContext(ctx).Raw(`SELECT
		EXISTS(SELECT 1 FROM pg_available_extensions WHERE name = 'pg_search'),
		EXISTS(SELECT 1 FROM pg_extension WHERE extname = 'pg_search'),
		COALESCE((SELECT extversion FROM pg_extension WHERE extname = 'pg_search'), ''),
		to_regclass('torrent_contents_bm25_idx') IS NOT NULL,
		COALESCE((SELECT indisvalid AND indisready FROM pg_index WHERE indexrelid = to_regclass('torrent_contents_bm25_idx')), false)`).
		Row().Scan(&status.ExtensionAvailable, &status.ExtensionInstalled, &status.ExtensionVersion, &status.IndexExists, &status.IndexValid); err != nil {
		return status, fmt.Errorf("paradedb bm25 status: probe extension: %w", err)
	}
	rows, err := db.WithContext(ctx).Raw(`SELECT attname, attgenerated FROM pg_attribute
		WHERE attrelid = to_regclass('torrent_contents') AND attname IN ('search_text', 'ep_text') AND NOT attisdropped`).Rows()
	if err != nil {
		return status, fmt.Errorf("paradedb bm25 status: probe columns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name, generated string
		if err := rows.Scan(&name, &generated); err != nil {
			return status, fmt.Errorf("paradedb bm25 status: probe columns: %w", err)
		}
		switch name {
		case "search_text":
			status.SearchTextColumn = true
			status.SearchTextGenerated = generated == "s"
		case "ep_text":
			status.EpisodeTextColumn = true
			status.EpisodeTextGenerated = generated == "s"
		}
	}
	if err := rows.Err(); err != nil {
		return status, fmt.Errorf("paradedb bm25 status: probe columns: %w", err)
	}
	status.Active = status.Mode != BM25ModeOff && status.ExtensionInstalled && status.SearchTextColumn && status.IndexValid
	return status, nil
}

const (
	bm25TsvTextFunctionSQL  = `CREATE OR REPLACE FUNCTION torrent_contents_tsv_text(tsvector) RETURNS text LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$ SELECT array_to_string(tsvector_to_array($1), ' ') $$`
	bm25EpTokensFunctionSQL = `CREATE OR REPLACE FUNCTION torrent_contents_ep_tokens(jsonb) RETURNS text LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
SELECT COALESCE(string_agg(tok, ' ' ORDER BY tok), '')
FROM (
 SELECT 's' || s.key || 'all' AS tok
 FROM jsonb_each(CASE WHEN jsonb_typeof($1)='object' THEN $1 ELSE '{}'::jsonb END) AS s(key, value)
 WHERE s.value='{}'::jsonb
 UNION ALL
 SELECT 's' || s.key || 'e' || e.key
 FROM jsonb_each(CASE WHEN jsonb_typeof($1)='object' THEN $1 ELSE '{}'::jsonb END) AS s(key, value)
 CROSS JOIN LATERAL jsonb_object_keys(CASE WHEN jsonb_typeof(s.value)='object' THEN s.value ELSE '{}'::jsonb END) AS e(key)
 WHERE s.value<>'{}'::jsonb
) t $$`
	bm25IndexFieldsSQL = `(id, (search_text::pdb.whitespace('lowercase=false')), (ep_text::pdb.whitespace), (content_type::pdb.literal), (video_resolution::pdb.literal), (video_codec::pdb.literal), (video_source::pdb.literal), is_anime, (content_source::pdb.literal), (content_id::pdb.literal), seeders, published_at)`
)

// EnableParadeDBBM25 idempotently installs the opt-in ParadeDB BM25 objects:
// the pg_search extension, the migration 00080 helper functions when missing,
// the generated search_text/ep_text columns, and a valid
// torrent_contents_bm25_idx. It must run outside a transaction so the index
// can be built concurrently.
func EnableParadeDBBM25(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("enable paradedb bm25: nil database handle")
	}
	var available bool
	if err := db.WithContext(ctx).Raw(`SELECT EXISTS(SELECT 1 FROM pg_available_extensions WHERE name = 'pg_search')`).Row().Scan(&available); err != nil {
		return fmt.Errorf("enable paradedb bm25: probe pg_search availability: %w", err)
	}
	if !available {
		return fmt.Errorf("enable paradedb bm25: pg_search extension is not available on this PostgreSQL server; use the paradedb/paradedb:0.26.6-pg18 image with shared_preload_libraries=pg_search,pg_cron")
	}
	if err := db.WithContext(ctx).Exec(`CREATE EXTENSION IF NOT EXISTS pg_search`).Error; err != nil {
		return fmt.Errorf("enable paradedb bm25: create pg_search extension: %w", err)
	}
	var hasTsvFn, hasEpFn bool
	if err := db.WithContext(ctx).Raw(`SELECT to_regprocedure('torrent_contents_tsv_text(tsvector)') IS NOT NULL, to_regprocedure('torrent_contents_ep_tokens(jsonb)') IS NOT NULL`).Row().Scan(&hasTsvFn, &hasEpFn); err != nil {
		return fmt.Errorf("enable paradedb bm25: probe helper functions: %w", err)
	}
	if !hasTsvFn {
		if err := db.WithContext(ctx).Exec(bm25TsvTextFunctionSQL).Error; err != nil {
			return fmt.Errorf("enable paradedb bm25: create torrent_contents_tsv_text: %w", err)
		}
	}
	if !hasEpFn {
		if err := db.WithContext(ctx).Exec(bm25EpTokensFunctionSQL).Error; err != nil {
			return fmt.Errorf("enable paradedb bm25: create torrent_contents_ep_tokens: %w", err)
		}
	}
	if err := db.WithContext(ctx).Exec(`ALTER TABLE torrent_contents
		ADD COLUMN IF NOT EXISTS search_text text GENERATED ALWAYS AS (torrent_contents_tsv_text(tsv)) STORED,
		ADD COLUMN IF NOT EXISTS ep_text text GENERATED ALWAYS AS (torrent_contents_ep_tokens(episodes)) STORED`).Error; err != nil {
		return fmt.Errorf("enable paradedb bm25: add generated columns: %w", err)
	}
	if err := createBM25Index(ctx, db); err != nil {
		return err
	}
	ResetDBCapabilitiesCache()
	status, err := ParadeDBBM25Status(ctx, db)
	if err != nil {
		return err
	}
	if !status.IndexValid || !status.SearchTextColumn || !status.EpisodeTextColumn {
		return fmt.Errorf("enable paradedb bm25: installation incomplete after DDL (index_valid=%v search_text=%v ep_text=%v)", status.IndexValid, status.SearchTextColumn, status.EpisodeTextColumn)
	}
	return nil
}

// RebuildParadeDBBM25 drops and rebuilds torrent_contents_bm25_idx with the
// current field definition (adding newly indexed filter columns such as
// video_codec, video_source, is_anime, content_source, content_id).
func RebuildParadeDBBM25(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("rebuild paradedb bm25: nil database handle")
	}
	if err := db.WithContext(ctx).Exec(`DROP INDEX IF EXISTS torrent_contents_bm25_idx`).Error; err != nil {
		return fmt.Errorf("rebuild paradedb bm25: drop existing index: %w", err)
	}
	return EnableParadeDBBM25(ctx, db)
}

func createBM25Index(ctx context.Context, db *gorm.DB) error {
	var exists, valid bool
	if err := db.WithContext(ctx).Raw(`SELECT to_regclass('torrent_contents_bm25_idx') IS NOT NULL, COALESCE((SELECT indisvalid AND indisready FROM pg_index WHERE indexrelid = to_regclass('torrent_contents_bm25_idx')), false)`).Row().Scan(&exists, &valid); err != nil {
		return fmt.Errorf("enable paradedb bm25: probe index: %w", err)
	}
	if exists && valid {
		return nil
	}
	if exists && !valid {
		if err := db.WithContext(ctx).Exec(`DROP INDEX IF EXISTS torrent_contents_bm25_idx`).Error; err != nil {
			return fmt.Errorf("enable paradedb bm25: drop invalid index: %w", err)
		}
	}
	createSQL := `CREATE INDEX CONCURRENTLY IF NOT EXISTS torrent_contents_bm25_idx ON torrent_contents USING bm25 ` + bm25IndexFieldsSQL
	if err := db.WithContext(ctx).Exec(createSQL).Error; err != nil {
		if !strings.Contains(err.Error(), "cannot run inside a transaction") {
			return fmt.Errorf("enable paradedb bm25: create index: %w", err)
		}
		plainSQL := `CREATE INDEX IF NOT EXISTS torrent_contents_bm25_idx ON torrent_contents USING bm25 ` + bm25IndexFieldsSQL
		if plainErr := db.WithContext(ctx).Exec(plainSQL).Error; plainErr != nil {
			return fmt.Errorf("enable paradedb bm25: create index: %w", plainErr)
		}
	}
	return nil
}

// DisableParadeDBBM25 removes the BM25 index and generated columns added by
// EnableParadeDBBM25. The pg_search extension and helper functions remain;
// both are inert on the stock execution path.
func DisableParadeDBBM25(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("disable paradedb bm25: nil database handle")
	}
	if err := db.WithContext(ctx).Exec(`DROP INDEX IF EXISTS torrent_contents_bm25_idx`).Error; err != nil {
		return fmt.Errorf("disable paradedb bm25: drop index: %w", err)
	}
	var tableExists bool
	if err := db.WithContext(ctx).Raw(`SELECT to_regclass('torrent_contents') IS NOT NULL`).Row().Scan(&tableExists); err != nil {
		return fmt.Errorf("disable paradedb bm25: probe torrent_contents: %w", err)
	}
	if tableExists {
		if err := db.WithContext(ctx).Exec(`ALTER TABLE torrent_contents DROP COLUMN IF EXISTS ep_text`).Error; err != nil {
			return fmt.Errorf("disable paradedb bm25: drop ep_text column: %w", err)
		}
		if err := db.WithContext(ctx).Exec(`ALTER TABLE torrent_contents DROP COLUMN IF EXISTS search_text`).Error; err != nil {
			return fmt.Errorf("disable paradedb bm25: drop search_text column: %w", err)
		}
	}
	ResetDBCapabilitiesCache()
	return nil
}

// BM25PlanCheck is one EXPLAIN verification of a representative BM25 query.
type BM25PlanCheck struct {
	Name         string `json:"name"`
	SQL          string `json:"sql"`
	Plan         string `json:"plan"`
	UsesBM25Scan bool   `json:"uses_bm25_scan"`
	UsesTopK     bool   `json:"uses_top_k"`
}

var bm25PlanQueries = []struct {
	name string
	sql  string
}{
	{
		name: "relevance",
		sql:  `SELECT torrent_contents.*, pdb.score(torrent_contents.id) AS _order_0 FROM torrent_contents WHERE torrent_contents.search_text &&& 'synthetic 1080p' ORDER BY _order_0 DESC LIMIT 10`,
	},
	{
		name: "relevance_seeders_published",
		sql:  `SELECT torrent_contents.*, pdb.score(torrent_contents.id) AS _order_0 FROM torrent_contents WHERE torrent_contents.search_text &&& 'synthetic 1080p' AND torrent_contents.seeders > 0 AND torrent_contents.published_at > '2000-01-01'::timestamptz ORDER BY _order_0 DESC LIMIT 10`,
	},
	{
		name: "relevance_content_type",
		sql:  `SELECT torrent_contents.*, pdb.score(torrent_contents.id) AS _order_0 FROM torrent_contents WHERE torrent_contents.search_text &&& 'synthetic 1080p' AND torrent_contents.content_type = 'movie' ORDER BY _order_0 DESC LIMIT 10`,
	},
	{
		name: "relevance_episodes",
		sql:  `SELECT torrent_contents.*, pdb.score(torrent_contents.id) AS _order_0 FROM torrent_contents WHERE torrent_contents.search_text &&& 'synthetic 1080p' AND torrent_contents.ep_text ||| 's1all s1e1' ORDER BY _order_0 DESC LIMIT 10`,
	},
}

// VerifyParadeDBBM25Plan runs EXPLAIN (COSTS OFF) on representative BM25
// relevance queries and confirms each uses a ParadeDB top-K scan rather than
// a sequential scan or GIN fallback.
func VerifyParadeDBBM25Plan(ctx context.Context, db *gorm.DB) ([]BM25PlanCheck, error) {
	status, err := ParadeDBBM25Status(ctx, db)
	if err != nil {
		return nil, err
	}
	if !status.ExtensionInstalled || !status.SearchTextColumn || !status.IndexValid {
		return nil, fmt.Errorf("verify paradedb bm25: BM25 is not enabled (extension_installed=%v search_text=%v index_valid=%v); run dh-waypoint search bm25 enable first", status.ExtensionInstalled, status.SearchTextColumn, status.IndexValid)
	}
	checks := make([]BM25PlanCheck, 0, len(bm25PlanQueries))
	for _, q := range bm25PlanQueries {
		rows, rowsErr := db.WithContext(ctx).Raw("EXPLAIN (COSTS OFF) " + q.sql).Rows()
		if rowsErr != nil {
			return checks, fmt.Errorf("verify paradedb bm25: explain %s: %w", q.name, rowsErr)
		}
		var lines []string
		for rows.Next() {
			var line string
			if scanErr := rows.Scan(&line); scanErr != nil {
				_ = rows.Close()
				return checks, fmt.Errorf("verify paradedb bm25: explain %s: %w", q.name, scanErr)
			}
			lines = append(lines, line)
		}
		rowsErr = rows.Err()
		_ = rows.Close()
		if rowsErr != nil {
			return checks, fmt.Errorf("verify paradedb bm25: explain %s: %w", q.name, rowsErr)
		}
		plan := strings.Join(lines, "\n")
		check := BM25PlanCheck{
			Name:         q.name,
			SQL:          q.sql,
			Plan:         plan,
			UsesBM25Scan: strings.Contains(plan, "ParadeDB Base Scan") && strings.Contains(plan, "torrent_contents_bm25_idx"),
			UsesTopK:     strings.Contains(plan, "TopKScanExecState"),
		}
		checks = append(checks, check)
		if !check.UsesBM25Scan || !check.UsesTopK {
			return checks, fmt.Errorf("verify paradedb bm25: plan check %s did not use a ParadeDB top-K scan:\n%s", q.name, plan)
		}
	}
	return checks, nil
}

func textArrayExpr(values []string) (string, []interface{}) {
	placeholders := make([]string, len(values))
	args := make([]interface{}, len(values))
	for i, v := range values {
		placeholders[i] = "?"
		args[i] = v
	}
	return "ARRAY[" + strings.Join(placeholders, ", ") + "]::text[]", args
}

// TorrentContentEpisodeKeysCriteria builds a GIN array-overlap/containment
// predicate matching torrent_contents_episode_keys_idx for resolved canonical
// TV keys and requested season/episode numbers. Scalar ARRAY[?]::text[]
// placeholders avoid GORM slice-to-tuple expansion, and explicit NOT NULL
// guards ensure partial-index subsumption on PostgreSQL 16+.
func TorrentContentEpisodeKeysCriteria(keys []model.ContentRef, episodes model.Episodes) query.Criteria {
	if len(keys) == 0 || len(episodes) == 0 {
		return query.And()
	}
	const partialGuard = "torrent_contents.content_id IS NOT NULL AND torrent_contents.episodes IS NOT NULL AND "
	const fnCall = "torrent_contents_episode_keys(torrent_contents.content_type, torrent_contents.content_source, torrent_contents.content_id, torrent_contents.episodes)"
	var perSeason []query.Criteria
	for _, entry := range episodes.SeasonEntries() {
		if len(entry.Episodes) == 0 {
			vals := make([]string, 0, len(keys))
			for _, k := range keys {
				vals = append(vals, fmt.Sprintf("%s:%s:%s:s:%d", k.Type, k.Source, k.ID, entry.Season))
			}
			arrSQL, args := textArrayExpr(vals)
			perSeason = append(perSeason, query.DBCriteria{
				SQL:  partialGuard + fnCall + " && " + arrSQL,
				Args: args,
			})
			continue
		}
		var orPerKey []query.Criteria
		for _, k := range keys {
			wholeKey := fmt.Sprintf("%s:%s:%s:s_all:%d", k.Type, k.Source, k.ID, entry.Season)
			epVals := make([]string, 0, len(entry.Episodes))
			for _, ep := range entry.Episodes {
				epVals = append(epVals, fmt.Sprintf("%s:%s:%s:e:%d:%d", k.Type, k.Source, k.ID, entry.Season, ep))
			}
			wholeSQL, wholeArgs := textArrayExpr([]string{wholeKey})
			epSQL, epArgs := textArrayExpr(epVals)
			orPerKey = append(orPerKey, query.DBCriteria{
				SQL:  partialGuard + "(" + fnCall + " @> " + wholeSQL + " OR " + fnCall + " @> " + epSQL + ")",
				Args: append(wholeArgs, epArgs...),
			})
		}
		perSeason = append(perSeason, query.Or(orPerKey...))
	}
	return query.And(perSeason...)
}
