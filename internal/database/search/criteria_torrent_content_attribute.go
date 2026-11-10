package search

import (
	"database/sql/driver"

	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/query"
	"github.com/spencernorton/DH-Waypoint/internal/maps"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"gorm.io/gen/field"
)

var VideoResolutionCriteria = torrentContentAttributeCriteria[model.VideoResolution](videoResolutionField)

var Video3DCriteria = torrentContentAttributeCriteria[model.Video3D](video3dField)

// VideoResolutionNotCriteria matches rows whose resolution is none of values,
// including rows with no parsed resolution, which a bare NOT IN would drop.
func VideoResolutionNotCriteria(values ...model.VideoResolution) query.Criteria {
	return query.DaoCriteria{
		Conditions: func(ctx query.DBContext) ([]field.Expr, error) {
			fld := videoResolutionField(ctx.Query())
			valuers := make([]driver.Valuer, 0, len(values))
			for _, v := range values {
				valuers = append(valuers, v)
			}
			return []field.Expr{field.Or(fld.IsNull(), field.Not(fld.In(valuers...)))}, nil
		},
		Joins: maps.NewInsertMap(
			maps.MapEntry[string, struct{}]{Key: model.TableNameTorrentContent},
		),
	}
}

func torrentContentAttributeCriteria[T attribute](getFld func(*dao.Query) field.Field) func(...T) query.Criteria {
	return func(values ...T) query.Criteria {
		return query.DaoCriteria{
			Conditions: func(ctx query.DBContext) ([]field.Expr, error) {
				q := ctx.Query()
				fld := getFld(q)
				valuers := make([]driver.Valuer, 0, len(values))
				for _, v := range values {
					valuers = append(valuers, v)
				}
				return []field.Expr{fld.In(valuers...)}, nil
			},
			Joins: maps.NewInsertMap(
				maps.MapEntry[string, struct{}]{Key: model.TableNameTorrentContent},
			),
		}
	}
}
