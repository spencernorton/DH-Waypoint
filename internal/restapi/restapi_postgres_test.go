package restapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
	g, err := gorm.Open(postgres.New(postgres.Config{Conn: db}), &gorm.Config{Logger: logger.Discard})
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
