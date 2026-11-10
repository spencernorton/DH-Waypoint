package search

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type dryRunContext struct{ q *dao.Query }

func (c dryRunContext) Query() *dao.Query                        { return c.q }
func (dryRunContext) TableName() string                          { return model.TableNameTorrentContent }
func (dryRunContext) NewSubQuery(context.Context) query.SubQuery { return nil }

// TestVideoResolutionNotCriteriaSQL locks the emitted SQL: a row with no parsed
// resolution must match, which NOT IN alone would not do (NULL NOT IN is NULL).
func TestVideoResolutionNotCriteriaSQL(t *testing.T) {
	t.Parallel()

	sqlDB, _, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	require.NoError(t, err)

	raw, err := VideoResolutionNotCriteria(model.VideoResolutionV720p, model.VideoResolutionV1080p).
		Raw(dryRunContext{q: dao.Use(db)})
	require.NoError(t, err)

	sql := db.Session(&gorm.Session{DryRun: true}).Model(&model.TorrentContent{}).
		Where("torrent_contents.content_type = ?", "tv_show").
		Where(raw.Query, raw.Args...).
		Find(&[]model.TorrentContent{}).Statement.SQL.String()
	assert.Contains(t, sql,
		`AND ("torrent_contents"."video_resolution" IS NULL OR "torrent_contents"."video_resolution" NOT IN ($2,$3))`)
	assert.Contains(t, raw.Joins.Keys(), model.TableNameTorrentContent)
}
