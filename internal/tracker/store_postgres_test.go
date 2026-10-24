package tracker

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	mi "github.com/anacrolix/torrent/metainfo"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencernorton/DH-Waypoint/internal/blocking"
	"github.com/spencernorton/DH-Waypoint/internal/csamblocklist"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/importer"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo"
	migrationssql "github.com/spencernorton/DH-Waypoint/migrations"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type passThroughBlocking struct{}

func (passThroughBlocking) Filter(_ context.Context, hs []protocol.InfoHash) ([]protocol.InfoHash, error) {
	return hs, nil
}
func (passThroughBlocking) Block(context.Context, []protocol.InfoHash, string, string, bool) error {
	return nil
}
func (passThroughBlocking) Flush(context.Context) error { return nil }

func trackerFixture(t *testing.T, cfg Config) (*Store, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL not configured")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("tracker_%d", time.Now().UnixNano())
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

	poolLazy := lazy.New(func() (*pgxpool.Pool, error) { return pool, nil })
	imp := importer.New(importer.Params{
		Dao:             lazy.New(func() (*dao.Query, error) { return dao.Use(g), nil }),
		CsamBlocklist:   csamblocklist.NewNoOp(),
		BlockingManager: lazy.New(func() (blocking.Manager, error) { return passThroughBlocking{}, nil }),
		Metrics:         importer.NewMetrics(),
	}).Importer
	store, err := NewStore(cfg, poolLazy, imp)
	require.NoError(t, err)
	return store, pool
}

func privateTorrentFile(t *testing.T) []byte {
	t.Helper()
	priv := true
	info := mi.Info{
		Name:        "Private.Release.2026",
		PieceLength: 16384,
		Private:     &priv,
		Files: []mi.FileInfo{
			{Length: 1000, Path: []string{"film.mkv"}},
			{Length: 50, Path: []string{"subs", "en.srt"}},
		},
		Pieces: bytes.Repeat([]byte{9}, 20),
	}
	infoBytes, err := bencode.Marshal(info)
	require.NoError(t, err)
	meta := mi.MetaInfo{InfoBytes: infoBytes, Announce: "https://old.example/announce"}
	var buf bytes.Buffer
	require.NoError(t, meta.Write(&buf))
	return buf.Bytes()
}

func TestPostgresUploadAnnounceDownload(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Passkeys = []string{"test-passkey-1"}
	cfg.AdminToken = "admin-secret"
	cfg.PublicBaseURL = "https://tracker.example.com"
	store, pool := trackerFixture(t, cfg)
	ctx := context.Background()

	raw := privateTorrentFile(t)
	result, err := store.Upload(ctx, raw)
	require.NoError(t, err)
	require.True(t, result.Private)
	require.False(t, result.Existed)
	hash, err := protocol.ParseID(result.InfoHash)
	require.NoError(t, err)

	var private bool
	var jobs int
	require.NoError(t, pool.QueryRow(ctx, `SELECT private FROM torrents WHERE info_hash=$1`, hash.Bytes()).Scan(&private))
	require.True(t, private)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM queue_jobs`).Scan(&jobs))
	require.Positive(t, jobs, "upload must enqueue core processing")

	detail, err := store.Detail(ctx, hash)
	require.NoError(t, err)
	require.True(t, detail.HasFile)
	require.Len(t, detail.Files, 2)

	stored, err := store.GetTorrentFile(ctx, hash)
	require.NoError(t, err)
	require.Equal(t, raw, stored)

	// Re-upload while the first processing job is still pending
	// refreshes the stored file instead of failing.
	again, err := store.Upload(ctx, raw)
	require.NoError(t, err)
	require.True(t, again.Existed)

	seeder := Announce{
		InfoHash: hash, PeerID: bytes.Repeat([]byte{1}, 20), Port: 6881,
		Left: 0, Compact: true, NumWant: 50,
	}
	resp, err := store.Announce(ctx, seeder, "192.0.2.10")
	require.NoError(t, err)
	require.Equal(t, 1, resp.Complete)
	require.Equal(t, 0, resp.Incomplete)

	leecher := Announce{
		InfoHash: hash, PeerID: bytes.Repeat([]byte{2}, 20), Port: 6882,
		Left: 500, Compact: true, NumWant: 50,
	}
	resp, err = store.Announce(ctx, leecher, "192.0.2.11")
	require.NoError(t, err)
	require.Equal(t, 1, resp.Complete)
	require.Equal(t, 1, resp.Incomplete)
	require.NotEmpty(t, resp.Peers, "leecher should learn the seeder")

	swarms, err := store.Scrape(ctx, []protocol.ID{hash, {42}})
	require.NoError(t, err)
	require.Len(t, swarms, 1)
	require.Equal(t, SwarmCounts{Seeders: 1, Leechers: 1, Completed: 1}, swarms[hash])

	leecher.Event = "stopped"
	_, err = store.Announce(ctx, leecher, "192.0.2.11")
	require.NoError(t, err)
	swarms, err = store.Scrape(ctx, []protocol.ID{hash})
	require.NoError(t, err)
	require.Equal(t, 0, swarms[hash].Leechers)

	// Re-announcing a completion never double-counts, and the
	// all-time total survives peer-row deletion (retention sweep).
	_, err = store.Announce(ctx, seeder, "192.0.2.10")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM tracker_peers WHERE info_hash=$1`, hash.Bytes())
	require.NoError(t, err)
	swarms, err = store.Scrape(ctx, []protocol.ID{hash})
	require.NoError(t, err)
	require.Equal(t, 0, swarms[hash].Seeders)
	require.Equal(t, 1, swarms[hash].Completed, "completed is an all-time total")

	_, err = store.Announce(ctx, Announce{InfoHash: protocol.ID{42}, PeerID: seeder.PeerID, Port: 1}, "192.0.2.1")
	require.ErrorIs(t, err, ErrUnregisteredTorrent)
}

func TestPostgresTrackerHTTP(t *testing.T) {
	cfg := NewDefaultConfig()
	cfg.Enabled = true
	cfg.Passkeys = []string{"test-passkey-1"}
	cfg.AdminToken = "admin-secret"
	cfg.PublicBaseURL = "https://tracker.example.com"
	store, _ := trackerFixture(t, cfg)
	ctx := context.Background()

	raw := privateTorrentFile(t)
	hash, _, err := ParseTorrentFile(raw)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	require.NoError(t, store.Apply(engine))

	// Upload without the admin token is refused; with it, accepted.
	req := httptest.NewRequest(http.MethodPost, "/api/private/v1/torrents", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	req = httptest.NewRequest(http.MethodPost, "/api/private/v1/torrents", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Download with a passkey personalises the announce URL and
	// preserves the info hash.
	req = httptest.NewRequest(http.MethodGet, "/api/private/v1/torrents/"+hash.String()+"/download?passkey=test-passkey-1", nil)
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	loaded, err := mi.Load(bytes.NewReader(rec.Body.Bytes()))
	require.NoError(t, err)
	require.Equal(t, "https://tracker.example.com/announce/test-passkey-1", loaded.Announce)
	_, err = metainfo.ParseTorrentFileInfo(hash, rec.Body.Bytes())
	require.NoError(t, err)

	// Announce over HTTP with a bad passkey yields a bencoded failure.
	q := url.Values{}
	q.Set("info_hash", string(hash.Bytes()))
	q.Set("peer_id", string(bytes.Repeat([]byte{5}, 20)))
	q.Set("port", "6881")
	q.Set("left", "0")
	req = httptest.NewRequest(http.MethodGet, "/announce/wrong-key?"+q.Encode(), nil)
	req.RemoteAddr = "192.0.2.20:12345"
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var failure map[string]any
	require.NoError(t, bencode.Unmarshal(rec.Body.Bytes(), &failure))
	require.Equal(t, "invalid passkey", failure["failure reason"])

	// And with the right passkey, a normal announce response.
	req = httptest.NewRequest(http.MethodGet, "/announce/test-passkey-1?"+q.Encode(), nil)
	req.RemoteAddr = "192.0.2.20:12345"
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var ok map[string]any
	require.NoError(t, bencode.Unmarshal(rec.Body.Bytes(), &ok))
	require.EqualValues(t, 1, ok["complete"])

	_ = ctx
}
