package queuemetrics

import (
	"context"
	"strings"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/metrics"
	"github.com/spencernorton/DH-Waypoint/internal/model"
	"gorm.io/gorm"
)

type Bucket struct {
	Queue           string
	Status          model.QueueJobStatus
	CreatedAtBucket time.Time
	RanAtBucket     time.Time
	Count           uint
	Latency         *time.Duration
}

type Request struct {
	BucketDuration metrics.BucketDuration
	Statuses       []model.QueueJobStatus
	Queues         []string
	StartTime      time.Time
	EndTime        time.Time
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
		req.BucketDuration,
	}

	var conditions []string
	if !win.Start.IsZero() {
		conditions = append(conditions, "(status != 'pending' OR created_at >= ?)")
		conditions = append(conditions, "(status = 'pending' OR ran_at >= ?)")
		params = append(params, win.Start, win.Start)
	}

	if !win.End.IsZero() {
		conditions = append(conditions, "(status != 'pending' OR created_at <= ?)")
		conditions = append(conditions, "(status = 'pending' OR ran_at <= ?)")
		params = append(params, win.End, win.End)
	}

	if req.Queues != nil {
		conditions = append(conditions, "queue IN ?")
		params = append(params, req.Queues)
	}

	if req.Statuses != nil {
		conditions = append(conditions, "status IN ?")
		params = append(params, req.Statuses)
	}

	conditionClause := ""
	if len(conditions) > 0 {
		conditionClause = "WHERE (" + strings.Join(conditions, " AND ") + ")"
	}
	params = append(params, metrics.MaxBuckets)

	var rawResult []rawBucket
	if err := c.db.WithContext(ctx).Raw(`select
        queue,
        status,
        created_at_bucket,
        ran_at_bucket,
        count,
        latency
        from (
            select
                queue,
                status,
                date_trunc(?, created_at) as created_at_bucket,
                date_trunc(?, ran_at) as ran_at_bucket,
                count(*) as count,
                (date_part('epoch', sum(ran_at-run_after)) * INTERVAL '1 second') as latency
            from queue_jobs
            `+conditionClause+`
            group by queue, status, created_at_bucket, ran_at_bucket
            order by coalesce(max(ran_at), max(created_at)) desc, created_at_bucket desc, queue asc, status asc
            limit ?
        ) bounded
        order by queue, status, created_at_bucket, ran_at_bucket`,
		params...,
	).Scan(&rawResult).Error; err != nil {
		return nil, err
	}

	result := make([]Bucket, len(rawResult))
	for i, raw := range rawResult {
		result[i] = raw.bucket()
	}

	return result, nil
}

type rawBucket struct {
	Queue           string
	Status          model.QueueJobStatus
	CreatedAtBucket time.Time
	RanAtBucket     time.Time
	Count           uint
	Latency         model.Duration
}

func (b rawBucket) bucket() Bucket {
	var latency *time.Duration

	if b.Latency > 0 {
		l := time.Duration(b.Latency)
		latency = &l
	}

	return Bucket{
		Queue:           b.Queue,
		Status:          b.Status,
		CreatedAtBucket: b.CreatedAtBucket,
		RanAtBucket:     b.RanAtBucket,
		Count:           b.Count,
		Latency:         latency,
	}
}
