package processor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	llmwork "github.com/spencernorton/DH-Waypoint/internal/legacyresults"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	migrationssql "github.com/spencernorton/DH-Waypoint/migrations"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func processorPostgresFixture(t *testing.T) (processor, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("disposable PostgreSQL fixture not configured")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	schema := fmt.Sprintf("processor_persist_%d", time.Now().UnixNano())
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize())
	require.NoError(t, err)
	require.NoError(t, admin.Close(ctx))

	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	db := stdlib.OpenDB(*cfg.ConnConfig)
	t.Cleanup(func() {
		_ = db.Close()
		pool.Close()
		c, cerr := pgx.Connect(context.Background(), dsn)
		if cerr == nil {
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
	pol, err := namepolicy.New(namepolicy.Config{Enabled: true})
	require.NoError(t, err)

	return processor{
		dao:        dao.Use(g),
		namePolicy: pol,
	}, pool
}

func TestPostgresPersistBatchedForUpdateAndGuardNameRows(t *testing.T) {
	p, pool := processorPostgresFixture(t)
	ctx := context.Background()

	h1 := protocol.InfoHash{0x10, 0x01}
	h2 := protocol.InfoHash{0x20, 0x02}
	h3 := protocol.InfoHash{0x30, 0x03}
	n1 := "Alpha.Horizon.2025.1080p.WEB-DL-GRP"
	n2 := "Beta.Horizon.2025.2160p.HEVC.HDR10-GRP"
	n3 := "Gamma.Adult.2025.1080p.mkv"

	for _, row := range []struct {
		h protocol.InfoHash
		n string
	}{
		{h1, n1},
		{h2, n2},
		{h3, n3},
	} {
		_, err := pool.Exec(ctx,
			`INSERT INTO torrents(info_hash,name,size,private,files_status,created_at,updated_at) VALUES($1,$2,1024,false,'no_info',now(),now())`,
			row.h.Bytes(), row.n,
		)
		require.NoError(t, err)
	}

	retainedAttrs := model.InferReleaseAttributes(n2, "2160p.HEVC.HDR10")
	retainedRaw, err := json.Marshal(retainedAttrs)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO torrent_contents(info_hash,content_type,release_attributes,created_at,updated_at) VALUES($1,'movie',$2,now(),now())`,
		h2.Bytes(), retainedRaw,
	)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO torrent_contents(info_hash,content_type,created_at,updated_at) VALUES($1,'xxx',now(),now())`,
		h3.Bytes(),
	)
	require.NoError(t, err)

	// Out-of-order multi-hash persist with namePolicy enabled must succeed in one batched tx
	// and preserve h2's existing ReleaseAttributes even when incoming ReleaseAttributes is nil.
	tc1 := model.TorrentContent{
		InfoHash:    h1,
		ContentType: model.NewNullContentType(model.ContentTypeMovie),
		Torrent:     model.Torrent{InfoHash: h1, Name: n1},
	}
	tc2 := model.TorrentContent{
		InfoHash:    h2,
		ContentType: model.NewNullContentType(model.ContentTypeMovie),
		Torrent:     model.Torrent{InfoHash: h2, Name: n2},
	}
	tc1.InferID()
	tc2.InferID()
	require.NoError(t, p.persist(ctx, persistPayload{
		torrentContents: []model.TorrentContent{tc2, tc1},
	}))

	var savedRaw []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT release_attributes FROM torrent_contents WHERE info_hash=$1`, h2.Bytes()).Scan(&savedRaw))
	var savedAttrs model.ReleaseAttributes
	require.NoError(t, json.Unmarshal(savedRaw, &savedAttrs))
	require.Equal(t, retainedAttrs.SourceNameSHA256, savedAttrs.SourceNameSHA256)

	// GuardNameRows must reject a batch containing an adult ('xxx') torrent_contents row.
	err = p.dao.Transaction(func(tx *dao.Query) error {
		return GuardNameRows(ctx, tx, p.namePolicy, []*model.TorrentContent{
			{InfoHash: h3, Torrent: model.Torrent{InfoHash: h3, Name: n3}},
			{InfoHash: h1, Torrent: model.Torrent{InfoHash: h1, Name: n1}},
		})
	})
	require.ErrorIs(t, err, llmwork.ErrHeld)

	// GuardNameRows must also reject a batch when a torrent name changed concurrently.
	err = p.dao.Transaction(func(tx *dao.Query) error {
		return GuardNameRows(ctx, tx, p.namePolicy, []*model.TorrentContent{
			{InfoHash: h1, Torrent: model.Torrent{InfoHash: h1, Name: "Stale.Name.2025.mkv"}},
		})
	})
	require.ErrorIs(t, err, llmwork.ErrHeld)
}
