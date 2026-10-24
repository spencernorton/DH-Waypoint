package adapter

import (
	"context"
	"fmt"
	"os"
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
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"
)

// Exercise the actual request options, SQL builder and PostgreSQL rather than
// asserting only the generated predicate text.
func TestRequestIdentifiersFilterBeforePaging(t *testing.T) {
	dsn := os.Getenv("DH_WAYPOINT_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set DH_WAYPOINT_TEST_POSTGRES_DSN to disposable PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("torznab_ids_%d", time.Now().UnixNano())
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
 id text PRIMARY KEY, info_hash bytea NOT NULL, content_type text,content_source text,content_id text);
 CREATE TABLE content_attributes (content_type text,content_source text,content_id text,source text,key text,value text);
 INSERT INTO torrent_contents VALUES
 ('tv',decode(repeat('01',20),'hex'),'tv_show','tmdb','42'),
 ('movie',decode(repeat('02',20),'hex'),'movie','tmdb','42'),
 ('unclassified',decode(repeat('03',20),'hex'),NULL,NULL,NULL),
 ('canonical-imdb',decode(repeat('04',20),'hex'),'movie','imdb','tt43');
 INSERT INTO content_attributes VALUES
 ('tv_show','tmdb','42','imdb','id','tt123'),
 ('tv_show','tmdb','42','tvdb','id','84'),
 ('movie','tmdb','42','imdb','id','tt456'),
 ('movie','tmdb','42','imdb','alt_title:0','tt999');`).Error)
	q := dao.Use(db)
	for _, tt := range []struct {
		name, kind, tmdb, imdb, tvdb string
		want                         []string
	}{
		{"canonical TV", "tvsearch", "42", "", "", []string{"tv"}},
		{"canonical movie", "movie", "42", "", "", []string{"movie"}},
		{"generic ID includes TV namespace", "search", "42", "", "", []string{"movie", "tv"}},
		{"IMDB numeric normalization", "tvsearch", "", "123", "", []string{"tv"}},
		{"IMDB canonical source", "movie", "", "tt43", "", []string{"canonical-imdb"}},
		{"TVDB mapped alias", "tvsearch", "", "", "84", []string{"tv"}},
		{"unknown alias does not veto known ID", "tvsearch", "42", "tt404", "", []string{"tv"}},
		{"TVDB does not enter movie namespace", "movie", "", "", "84", nil},
		{"non identifier attribute", "movie", "", "tt999", "", nil},
		{"bound SQL input", "movie", "42' OR true --", "", "", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := torznab.SearchRequest{Type: tt.kind, Profile: torznab.ProfileDefault}
			if tt.tmdb != "" {
				req.TMDBID = model.NewNullString(tt.tmdb)
			}
			if tt.imdb != "" {
				req.IMDBID = model.NewNullString(tt.imdb)
			}
			if tt.tvdb != "" {
				req.TVDBID = model.NewNullString(tt.tvdb)
			}
			opts, err := searchRequestToQueryOptions(req)
			require.NoError(t, err)
			// A stable independent order makes LIMIT/OFFSET observable.
			opts = append(opts, query.SelectAll(), search.TorrentContentCoreJoins(), query.OrderBy(query.OrderByColumn{
				OrderByColumn: clause.OrderByColumn{Column: clause.Column{Table: "torrent_contents", Name: "id"}},
			}))
			result, err := query.GenericQuery[model.TorrentContent](ctx, q, query.Options(opts...),
				model.TableNameTorrentContent, func(ctx context.Context, q *dao.Query) query.SubQuery {
					return query.GenericSubQuery[dao.ITorrentContentDo]{SubQuery: q.TorrentContent.WithContext(ctx).ReadDB()}
				})
			require.NoError(t, err)
			var ids []string
			for _, item := range result.Items {
				ids = append(ids, item.ID)
			}
			require.ElementsMatch(t, tt.want, ids)
			for offset := 0; offset <= len(result.Items); offset++ {
				pageOpts := append(append([]query.Option{}, opts...), query.Limit(1), query.Offset(uint(offset)), query.WithHasNextPage(true))
				page, err := query.GenericQuery[model.TorrentContent](ctx, q, query.Options(pageOpts...), model.TableNameTorrentContent,
					func(ctx context.Context, q *dao.Query) query.SubQuery {
						return query.GenericSubQuery[dao.ITorrentContentDo]{SubQuery: q.TorrentContent.WithContext(ctx).ReadDB()}
					})
				require.NoError(t, err)
				if offset < len(result.Items) {
					require.Len(t, page.Items, 1)
					require.Equal(t, result.Items[offset].ID, page.Items[0].ID)
				} else {
					require.Empty(t, page.Items)
				}
			}
		})
	}
}
