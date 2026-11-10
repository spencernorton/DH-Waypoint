package adapter

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/spencernorton/DH-Waypoint/internal/torznab"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Runs the real request options against PostgreSQL: every film and TV row,
// whatever its resolution and with none at all, must be found under exactly
// one of SD and HD, and must report the category it was found under.
func TestResolutionCategoriesCoverEveryRow(t *testing.T) {
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set DH_WAYPOINT_TEST_POSTGRES_DSN to disposable PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("torznab_res_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted)
	require.NoError(t, err)
	t.Cleanup(func() { _, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); require.NoError(t, err) })
	cfg, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.RuntimeParams["search_path"] = schema
	sqlDB := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: gormlogger.Discard})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`CREATE TABLE torrent_contents (
 id text PRIMARY KEY, info_hash bytea NOT NULL, content_type text, video_resolution text)`).Error)

	// One row per content type and resolution, plus one with no resolution.
	resolutions := append([]string{""}, model.VideoResolutionNames()...)
	for _, contentType := range []string{"tv_show", "movie"} {
		for _, res := range resolutions {
			require.NoError(t, db.Exec(
				`INSERT INTO torrent_contents VALUES (?, decode(repeat('01',20),'hex'), ?, NULLIF(?, ''))`,
				contentType+"/"+res, contentType, res,
			).Error)
		}
	}

	q := dao.Use(db)
	find := func(function string, cats ...int) []model.TorrentContent {
		opts, err := searchRequestToQueryOptions(
			torznab.SearchRequest{Type: function, Cats: cats, Profile: torznab.ProfileDefault})
		require.NoError(t, err)
		opts = append(opts, query.SelectAll(), search.TorrentContentCoreJoins())
		result, err := query.GenericQuery[model.TorrentContent](ctx, q, query.Options(opts...),
			model.TableNameTorrentContent, func(ctx context.Context, q *dao.Query) query.SubQuery {
				return query.GenericSubQuery[dao.ITorrentContentDo]{SubQuery: q.TorrentContent.WithContext(ctx).ReadDB()}
			})
		require.NoError(t, err)
		return result.Items
	}
	ids := func(rows []model.TorrentContent) []string {
		out := make([]string, 0, len(rows))
		for _, row := range rows {
			out = append(out, row.ID)
		}
		return out
	}

	for _, tt := range []struct {
		contentType string
		function    string
		cats        struct{ parent, sd, hd, uhd int }
	}{
		{"movie", torznab.FunctionMovie, resolutionCategories[0]},
		{"tv_show", torznab.FunctionTV, resolutionCategories[1]},
	} {
		// The generic function has to add the content type itself; the typed one already has it.
		for _, function := range []string{torznab.FunctionSearch, tt.function} {
			t.Run(tt.contentType+"/"+function, func(t *testing.T) {
				var all, wantSD, wantHD, wantUHD []string
				for _, res := range resolutions {
					id := tt.contentType + "/" + res
					all = append(all, id)
					switch res {
					case "", "V360p", "V480p", "V540p", "V576p":
						wantSD = append(wantSD, id)
					case "V2160p", "V4320p":
						wantUHD = append(wantUHD, id)
						fallthrough
					default:
						wantHD = append(wantHD, id)
					}
				}

				require.ElementsMatch(t, all, ids(find(function, tt.cats.parent)))
				require.ElementsMatch(t, wantSD, ids(find(function, tt.cats.sd)))
				require.ElementsMatch(t, wantHD, ids(find(function, tt.cats.hd)))
				require.ElementsMatch(t, wantUHD, ids(find(function, tt.cats.uhd)))
				// Sonarr's default for a hand-added indexer is the SD and HD pair.
				require.ElementsMatch(t, all, ids(find(function, tt.cats.sd, tt.cats.hd)))

				for _, cat := range []int{tt.cats.parent, tt.cats.sd, tt.cats.hd, tt.cats.uhd} {
					for _, row := range find(function, cat) {
						item := torrentContentResultItemToTorznabResultItem(
							search.TorrentContentResultItem{TorrentContent: row}, false)
						require.Contains(t, categoryValues(item), strconv.Itoa(cat), row.ID)
					}
				}
			})
		}
	}
}
