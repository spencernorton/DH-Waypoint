package processors

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Scheduling hints are private to a worker process. Each process starts with
// the oldest due job and reserves every other successful claim for that order.
// Hints never grant a lease or alter a prepared job, a retry time or a review.
type dispatchOrder struct {
	gate       chan struct{}
	mappedTurn bool
	mapped     []string
	retryAfter time.Time
}

func (order *dispatchOrder) claimed(id string) {
	if i := slices.Index(order.mapped, id); i >= 0 {
		order.mapped = slices.Delete(order.mapped, i, i+1)
	} else if order.mappedTurn {
		// Another process, epoch change or retry may have consumed every hint.
		order.mapped = nil
	}
	order.mappedTurn = !order.mappedTurn
}

// Look up a bounded batch outside both the capacity reservation and the epoch
// lock. The retained source is the snapshot taken before processor execution.
func (s *Store) loadMappedJobs(ctx context.Context) error {
	if len(s.dispatch.mapped) != 0 || time.Now().Before(s.dispatch.retryAfter) {
		return nil
	}
	p, err := s.pool.Get()
	if err != nil {
		return err
	}
	hintCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	rows, err := p.Query(hintCtx, `WITH ready AS MATERIALIZED (
 SELECT job_id,source,available_at,created_at FROM processor_jobs
 WHERE epoch=(SELECT epoch FROM processor_control WHERE singleton)
 AND state IN('queued','deferred') AND available_at<=statement_timestamp()
 ORDER BY available_at,created_at,job_id LIMIT $1
) SELECT job_id FROM ready WHERE source #>> '{contents,0,content_type}' IN ('movie','tv_show')
 AND source #>> '{contents,0,content_source}' IN ('tmdb','tvdb')
 AND coalesce(source #>> '{contents,0,content_id}','')<>''
 ORDER BY available_at,created_at,job_id LIMIT 32`, min(1000, s.cfg.QueueCapacity))
	if err != nil {
		return err
	}
	s.dispatch.mapped, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err == nil && len(s.dispatch.mapped) == 0 {
		s.dispatch.retryAfter = time.Now().Add(s.cfg.PollInterval)
	}
	return err
}
