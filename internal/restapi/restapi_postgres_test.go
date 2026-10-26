package restapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	migrationssql "github.com/spencernorton/DH-Waypoint/migrations"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func restEngine(t *testing.T) (*gin.Engine, *pgxpool.Pool) {
	t.Helper()
	return restEngineWithLogger(t, logger.Discard)
}

func restEngineWithLogger(t *testing.T, gl logger.Interface) (*gin.Engine, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL not configured")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("restapi_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	require.NoError(t, admin.Close(ctx))
	pcfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	pcfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pcfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	require.NoError(t, err)
	db := stdlib.OpenDB(*pcfg.ConnConfig)
	t.Cleanup(func() {
		_ = db.Close()
		pool.Close()
		c, err := pgx.Connect(context.Background(), dsn)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			_ = c.Close(context.Background())
		}
	})
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationssql.FS)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
	g, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{Logger: gl})
	require.NoError(t, err)

	policy, err := serving.NewPolicy(serving.Config{ExcludeAdult: true}, []string{"siterip"}, []string{"mkv", "jpg"})
	require.NoError(t, err)
	result := search.New(search.Params{
		Query:         lazy.New(func() (*dao.Query, error) { return dao.Use(g), nil }),
		ServingPolicy: policy,
	})
	api := New(result.ServingSearch, nil)
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	require.NoError(t, api.Apply(engine))
	return engine, pool
}

func TestRESTSearchDetailAndPrivacy(t *testing.T) {
	engine, pool := restEngine(t)
	ctx := context.Background()
	public := protocol.InfoHash{0x11}
	private := protocol.InfoHash{0x22}
	for _, seed := range []struct {
		hash protocol.InfoHash
		name string
		priv bool
	}{{public, "Public Film 2026", false}, {private, "Private Film 2026", true}} {
		_, err := pool.Exec(ctx, `INSERT INTO torrents(info_hash, name, size, private, files_status, created_at, updated_at)
			VALUES($1,$2,1000,$3,'single',clock_timestamp(),clock_timestamp())`, seed.hash.Bytes(), seed.name, seed.priv)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO torrent_contents(info_hash, content_type, size, published_at, created_at, updated_at)
			VALUES($1,'movie',1000,clock_timestamp(),clock_timestamp(),clock_timestamp())`, seed.hash.Bytes())
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO torrent_files(info_hash, index, path, size, created_at, updated_at)
			VALUES($1,0,'film.mkv',1000,clock_timestamp(),clock_timestamp())`, seed.hash.Bytes())
		require.NoError(t, err)
	}

	get := func(path string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var body map[string]any
		if rec.Body.Len() > 0 {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		}
		return rec.Code, body
	}

	code, body := get("/api/v1/search")
	require.Equal(t, 200, code)
	require.EqualValues(t, 1, body["total_count"], "private torrent must not appear")
	items := body["items"].([]any)
	require.Len(t, items, 1)
	require.Equal(t, public.String(), items[0].(map[string]any)["info_hash"])

	code, body = get("/api/v1/torrents/" + public.String())
	require.Equal(t, 200, code)
	require.Equal(t, "movie", body["content_type"])

	code, _ = get("/api/v1/torrents/" + private.String())
	require.Equal(t, 404, code, "private torrent detail must 404")

	code, body = get("/api/v1/torrents/" + public.String() + "/files")
	require.Equal(t, 200, code)
	require.Len(t, body["files"].([]any), 1)

	code, _ = get("/api/v1/search?limit=101")
	require.Equal(t, 400, code)
	code, _ = get("/api/v1/search?type=bogus")
	require.Equal(t, 400, code)

	code, body = get("/api/v1/stats")
	require.Equal(t, 200, code)
	require.EqualValues(t, 1, body["torrent_contents"])
}

type sqlCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *sqlCapture) Printf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(&c.buf, format, args...)
}

func (c *sqlCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *sqlCapture) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Reset()
}

func v2SearchHashes(t *testing.T, engine *gin.Engine, path string) (hashes []string, hasNext bool, nextAfter any) {
	t.Helper()
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Items []struct {
			InfoHash string `json:"info_hash"`
		} `json:"items"`
		HasNextPage bool `json:"has_next_page"`
		NextAfter   any  `json:"next_after"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	for _, item := range body.Items {
		hashes = append(hashes, item.InfoHash)
	}
	return hashes, body.HasNextPage, body.NextAfter
}

func TestV2SearchSubstringOrderingAndCursor(t *testing.T) {
	engine, pool := restEngine(t)
	ctx := context.Background()
	matching := []protocol.InfoHash{{0x30}, {0x10}, {0x50}, {0x20}, {0x40}}
	want := make([]string, 0, len(matching))
	for _, h := range matching {
		want = append(want, h.String())
	}
	sort.Strings(want)
	seed := func(hash protocol.InfoHash, name string, priv bool) {
		_, err := pool.Exec(ctx, `INSERT INTO torrents(info_hash, name, size, private, files_status, created_at, updated_at)
			VALUES($1,$2,1000,$3,'single',clock_timestamp(),clock_timestamp())`, hash.Bytes(), name, priv)
		require.NoError(t, err)
	}
	for i, h := range matching {
		seed(h, fmt.Sprintf("Catalogue zzrarezz %d", i), false)
	}
	seed(protocol.InfoHash{0x05}, "Ordinary Film", false)
	seed(protocol.InfoHash{0x60}, "Another Release", false)
	seed(protocol.InfoHash{0x15}, "Catalogue zzrarezz private", true)

	var got []string
	path := "/api/v2/search?q=zzrarezz&limit=2"
	for page := 0; ; page++ {
		hashes, hasNext, nextAfter := v2SearchHashes(t, engine, path)
		got = append(got, hashes...)
		if page < 2 {
			require.True(t, hasNext, "page %d", page)
			require.Equal(t, hashes[len(hashes)-1], nextAfter)
			path = "/api/v2/search?q=zzrarezz&limit=2&after=" + nextAfter.(string)
		} else {
			require.False(t, hasNext)
			require.Nil(t, nextAfter)
			break
		}
		require.Less(t, page, 3, "cursor pagination must terminate")
	}
	require.Equal(t, want, got, "substring pages must be info_hash ordered, complete and private-free")

	browse, hasNext, _ := v2SearchHashes(t, engine, "/api/v2/search?limit=100")
	require.False(t, hasNext)
	require.True(t, slices.IsSorted(browse))
	require.Len(t, browse, len(matching)+2)
}

func TestV2SearchSubstringGenericPlanUsesTrigramIndex(t *testing.T) {
	capture := &sqlCapture{}
	engine, pool := restEngineWithLogger(t, logger.New(capture, logger.Config{LogLevel: logger.Info, IgnoreRecordNotFoundError: true}))
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO torrents(info_hash, name, size, private, files_status, created_at, updated_at)
		SELECT decode(md5(i::text)||substring(md5('synthetic-'||i::text),1,8), 'hex'),
			'Synthetic Release '||i, 1000, false, 'single', clock_timestamp(), clock_timestamp()
		FROM generate_series(1, 2000) i`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE torrents SET name = 'Synthetic zzrarezz Release'
		WHERE info_hash IN (SELECT info_hash FROM torrents ORDER BY info_hash LIMIT 3)`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `ANALYZE torrents`)
	require.NoError(t, err)

	hashes, _, _ := v2SearchHashes(t, engine, "/api/v2/search?q=zzrarezz&limit=100")
	require.Len(t, hashes, 3)
	substringSQL := capture.String()
	require.Contains(t, substringSQL, "torrents.info_hash || ''::bytea")

	capture.Reset()
	_, _, _ = v2SearchHashes(t, engine, "/api/v2/search?limit=100")
	browseSQL := capture.String()
	require.Contains(t, browseSQL, `"torrents"."info_hash" AS _order_0`)
	require.NotContains(t, browseSQL, "|| ''::bytea")

	// Mirror the handleV2Search substring query (full serving policy
	// replaced by its private-row check) as a prepared statement, so the
	// generic plan cannot learn that the substring is rare.
	prepared := `SELECT torrents.info_hash, torrents.name, torrents.size, torrents.files_status, torrents.files_count, torrents.created_at, torrents.updated_at
		FROM torrents WHERE torrents.name ILIKE $1 AND NOT torrents.private
		ORDER BY torrents.info_hash || ''::bytea LIMIT 101`

	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, "SET LOCAL plan_cache_mode = force_generic_plan")
	require.NoError(t, err)
	_, err = tx.Exec(ctx, "SET LOCAL enable_seqscan = off")
	require.NoError(t, err)
	_, err = tx.Exec(ctx, "PREPARE v2_substring_probe(text) AS "+prepared)
	require.NoError(t, err)
	var raw []byte
	err = tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) EXECUTE v2_substring_probe('%zzrarezz%')").Scan(&raw)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback(ctx))

	var plans []map[string]any
	require.NoError(t, json.Unmarshal(raw, &plans))
	usesTrigram := false
	var visit func(node map[string]any)
	visit = func(node map[string]any) {
		if node["Index Name"] == "torrents_name_trgm_v2_idx" {
			usesTrigram = true
		}
		require.False(t, node["Index Name"] == "torrents_pkey" &&
			(node["Node Type"] == "Index Scan" || node["Node Type"] == "Index Only Scan"),
			"generic substring plan must not walk torrents_pkey: %v", node)
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				if childNode, ok := child.(map[string]any); ok {
					visit(childNode)
				}
			}
		}
	}
	visit(plans[0]["Plan"].(map[string]any))
	require.True(t, usesTrigram, "generic substring plan must use torrents_name_trgm_v2_idx: %s", raw)
}
