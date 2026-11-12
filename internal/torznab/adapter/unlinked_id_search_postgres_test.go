package adapter

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/migrations"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/protocol"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	"github.com/spencernorton/DH-Waypoint/internal/torznab"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

type staticDeadSet map[string]struct{}

func (d staticDeadSet) DeadSet(_ context.Context, _ [][]byte) (map[string]struct{}, error) {
	return d, nil
}

type staticBlockedSet map[string]struct{}

func (b staticBlockedSet) BlockedSet(_ context.Context, _ [][]byte) (map[string]struct{}, error) {
	return b, nil
}

func TestTorznabAdapterIDSearchIncludesUnlinkedMatchesEndToEnd(t *testing.T) {
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set DH_WAYPOINT_TEST_POSTGRES_DSN to disposable PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(admin.Close)

	_, err = admin.Exec(ctx, `SELECT pg_advisory_lock(830571);CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;CREATE EXTENSION IF NOT EXISTS btree_gin WITH SCHEMA public;SELECT pg_advisory_unlock(830571)`)
	require.NoError(t, err)

	schema := fmt.Sprintf("torznab_unlinked_e2e_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, dropErr := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE")
		require.NoError(t, dropErr)
	})

	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.RuntimeParams["search_path"] = schema + ",public"
	sqlDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })

	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: gormlogger.Discard})
	require.NoError(t, err)

	mig, err := migrations.New(migrations.Params{
		DB:     lazy.New(func() (*gorm.DB, error) { return db, nil }),
		Logger: zap.NewNop().Sugar(),
	}).Migrator.Get()
	require.NoError(t, err)
	require.NoError(t, mig.Up(ctx))

	q := dao.Use(db)
	policy, err := serving.NewPolicy(serving.Config{ExcludeAdult: true}, []string{"siterip"}, []string{"mkv"})
	require.NoError(t, err)
	sr := search.New(search.Params{
		Query:         lazy.New(func() (*dao.Query, error) { return q, nil }),
		ServingPolicy: policy,
	})
	servingSearch, err := sr.ServingSearch.Get()
	require.NoError(t, err)

	// Seed metadata sources and canonical TV show ("The Lantern", TMDB 7001, IMDb tt7001000, TVDB 507001).
	for _, src := range []model.MetadataSource{
		{Key: "tmdb", Name: "TMDB"},
		{Key: "imdb", Name: "IMDb"},
		{Key: "tvdb", Name: "TVDB"},
	} {
		require.NoError(t, db.Save(&src).Error)
	}
	require.NoError(t, db.Save(&model.TorrentSource{Key: "dht", Name: "DHT"}).Error)

	show := model.Content{
		Type:        model.ContentTypeTvShow,
		Source:      "tmdb",
		ID:          "7001",
		Title:       "The Lantern",
		ReleaseYear: 2022,
		Adult:       model.NewNullBool(false),
		Attributes: []model.ContentAttribute{
			{ContentType: model.ContentTypeTvShow, ContentSource: "tmdb", ContentID: "7001", Source: "imdb", Key: "id", Value: "tt7001000"},
			{ContentType: model.ContentTypeTvShow, ContentSource: "tmdb", ContentID: "7001", Source: "tvdb", Key: "id", Value: "507001"},
		},
	}
	show.UpdateTsv()
	require.NoError(t, db.Create(&show).Error)

	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	mkTorrent := func(idx int, name string, linked bool, eps model.Episodes, res model.VideoResolution, seeders uint, pubOffsetMin int) protocol.InfoHash {
		raw, decErr := hex.DecodeString(fmt.Sprintf("%040x", 0xa000+idx))
		require.NoError(t, decErr)
		var h protocol.InfoHash
		copy(h[:], raw)
		pub := now.Add(time.Duration(pubOffsetMin) * time.Minute)
		tor := model.Torrent{
			InfoHash:    h,
			Name:        name,
			Size:        1_500_000_000,
			FilesStatus: model.FilesStatusSingle,
			Extension:   model.NewNullString("mkv"),
			Sources: []model.TorrentsTorrentSource{
				{
					Source:      "dht",
					InfoHash:    h,
					Seeders:     model.NewNullUint(seeders),
					PublishedAt: sql.NullTime{Time: pub, Valid: true},
					UpdatedAt:   pub,
				},
			},
		}
		require.NoError(t, db.Create(&tor).Error)

		tc := model.TorrentContent{
			InfoHash:        h,
			Torrent:         tor,
			ContentType:     model.NewNullContentType(model.ContentTypeTvShow),
			Episodes:        eps,
			VideoResolution: model.NewNullVideoResolution(res),
			Seeders:         model.NewNullUint(seeders),
			PublishedAt:     pub,
		}
		if linked {
			tc.ContentSource = model.NewNullString("tmdb")
			tc.ContentID = model.NewNullString("7001")
			tc.Content = show
		}
		tc.ID = tc.InferID()
		tc.UpdateTsv()
		require.NoError(t, db.Create(&tc).Error)
		return h
	}

	s03e01 := make(model.Episodes).AddEpisode(3, 1)
	s03pack := make(model.Episodes).AddSeason(3)
	s03e02 := make(model.Episodes).AddEpisode(3, 2)

	hLinked := mkTorrent(1, "The.Lantern.S03E01.720p.WEB.h264-SYNTH", true, s03e01, model.VideoResolutionV720p, 12, 5)
	hUnlinkedEp := mkTorrent(2, "The.Lantern.S03E01.1080p.WEB.h264-SYNTH", false, s03e01, model.VideoResolutionV1080p, 40, 30)
	hUnlinkedPack := mkTorrent(3, "The.Lantern.S03.COMPLETE.1080p.WEB-DL-SYNTH", false, s03pack, model.VideoResolutionV1080p, 25, 20)
	_ = mkTorrent(4, "Babies.and.the.Lantern.S03E01.1080p.WEB.h264-SYNTH", false, s03e01, model.VideoResolutionV1080p, 50, 40)
	_ = mkTorrent(5, "The.Lantern.S03E02.1080p.WEB.h264-SYNTH", false, s03e02, model.VideoResolutionV1080p, 50, 45)
	hDead := mkTorrent(6, "The.Lantern.S03E01.2160p.WEB-DL-DEAD", false, s03e01, model.VideoResolutionV2160p, 15, 50)
	hBlocked := mkTorrent(7, "The.Lantern.S03E01.1080p.WEB-DL-BLOCKED", false, s03e01, model.VideoResolutionV1080p, 15, 55)
	_ = mkTorrent(8, "The.Lantern.S03E01.1080p.WEB-DL-ZEROSEED", false, s03e01, model.VideoResolutionV1080p, 0, 60)

	ad := NewWithFiltersAndRanker(
		servingSearch,
		staticDeadSet{hDead.String(): {}},
		true,
		nil,
		FreshnessConfig{HideZeroSeeders: true},
		reverseReranker{},
	).WithVerdicts(staticBlockedSet{hBlocked.String(): {}}, true, nil)

	for _, tcReq := range []struct {
		name string
		req  torznab.SearchRequest
	}{
		{
			name: "IMDb + TMDB Sonarr round 1",
			req: torznab.SearchRequest{
				Type:    torznab.FunctionTV,
				IMDBID:  model.NewNullString("tt7001000"),
				TMDBID:  model.NewNullString("7001"),
				Season:  model.NewNullInt(3),
				Episode: model.NewNullInt(1),
				Cats:    []int{torznab.CategoryTVSD.ID, torznab.CategoryTVHD.ID},
				Profile: torznab.ProfileDefault,
			},
		},
		{
			name: "TVDB only",
			req: torznab.SearchRequest{
				Type:    torznab.FunctionTV,
				TVDBID:  model.NewNullString("507001"),
				Season:  model.NewNullInt(3),
				Episode: model.NewNullInt(1),
				Cats:    []int{torznab.CategoryTVSD.ID, torznab.CategoryTVHD.ID},
				Profile: torznab.ProfileDefault,
			},
		},
		{
			name: "TMDB only",
			req: torznab.SearchRequest{
				Type:    torznab.FunctionTV,
				TMDBID:  model.NewNullString("7001"),
				Season:  model.NewNullInt(3),
				Episode: model.NewNullInt(1),
				Cats:    []int{torznab.CategoryTVSD.ID, torznab.CategoryTVHD.ID},
				Profile: torznab.ProfileDefault,
			},
		},
	} {
		t.Run(tcReq.name, func(t *testing.T) {
			res, sErr := ad.Search(ctx, tcReq.req)
			require.NoError(t, sErr)
			require.Len(t, res.Channel.Items, 3)
			// Linked item is ALWAYS first; reverseReranker reversed only within the unlinked tier!
			require.Equal(t, hLinked.String(), infoHashAttr(res.Channel.Items[0]))
			require.Equal(t, hUnlinkedPack.String(), infoHashAttr(res.Channel.Items[1]))
			require.Equal(t, hUnlinkedEp.String(), infoHashAttr(res.Channel.Items[2]))
		})
	}
}

func infoHashAttr(item torznab.SearchResultItem) string {
	for _, a := range item.TorznabAttrs {
		if a.AttrName == torznab.AttrInfoHash {
			return a.AttrValue
		}
	}
	return item.GUID
}
