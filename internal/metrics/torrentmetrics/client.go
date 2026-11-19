package torrentmetrics

import (
	"context"
	"strings"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/metrics"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"gorm.io/gorm"
)

type Bucket struct {
	Source  string
	Bucket  time.Time
	Updated bool
	Count   uint
}

type Request struct {
	BucketDuration metrics.BucketDuration
	Sources        []string
	StartTime      time.Time
	EndTime        time.Time
	Updated        model.NullBool
}

type Client interface {
	Request(context.Context, Request) ([]Bucket, error)
}

type client struct {
	db  *gorm.DB
	now func() time.Time
}

func (c client) Request(ctx context.Context, req Request) ([]Bucket, error) {
	nowFn := c.now
	if nowFn == nil {
		nowFn = time.Now
	}
	win, err := metrics.ResolveWindow(req.BucketDuration, req.StartTime, req.EndTime, nowFn())
	if err != nil {
		return nil, err
	}
	if win.Empty {
		return []Bucket{}, nil
	}

	params := []any{
		req.BucketDuration,
	}

	var conditions []string
	if !win.Start.IsZero() {
		conditions = append(conditions, "updated_at >= ?")
		params = append(params, win.Start)
	}

	if !win.End.IsZero() {
		conditions = append(conditions, "updated_at <= ?")
		params = append(params, win.End)
	}

	if req.Sources != nil {
		conditions = append(conditions, "source IN ?")
		params = append(params, req.Sources)
	}

	if req.Updated.Valid {
		sign := ">"
		if !req.Updated.Bool {
			sign = "<="
		}

		conditions = append(conditions, "updated_at "+sign+" (created_at + interval '1 hour')")
	}

	conditionClause := ""
	if len(conditions) > 0 {
		conditionClause = "WHERE (" + strings.Join(conditions, " AND ") + ")"
	}
	params = append(params, metrics.MaxBuckets)

	var result []Bucket
	if err := c.db.WithContext(ctx).Raw(`select
        source,
        bucket,
        updated,
        count
        from (
            select
                source,
                date_trunc(?, updated_at) as bucket,
                updated_at > (created_at + interval '1 hour') as updated,
                count(*) as count
            from torrents_torrent_sources
            `+conditionClause+`
            group by source, bucket, updated
            order by bucket desc, source asc, updated asc
            limit ?
        ) bounded
        order by source, bucket, updated`,
		params...,
	).Scan(&result).Error; err != nil {
		return nil, err
	}

	return result, nil
}
