package eventprune

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencernorton/DH-Waypoint/internal/catalogueguard"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const (
	workerKey = "eventprune"
	// maxBatches bounds one cycle's work per table, so a first run against a
	// large backlog is spread over many cycles.
	maxBatches = 20
)

type Params struct {
	fx.In
	Config Config
	Pool   lazy.Lazy[*pgxpool.Pool]
	Logger *zap.SugaredLogger
}

type Result struct {
	fx.Out
	Worker worker.Worker `group:"workers"`
}

func New(p Params) Result {
	w := &pruner{cfg: p.Config, pool: p.Pool, logger: p.Logger.Named(workerKey)}
	return Result{Worker: worker.NewWorker(workerKey, fx.Hook{OnStart: w.start, OnStop: w.stop})}
}

type pruner struct {
	cfg    Config
	pool   lazy.Lazy[*pgxpool.Pool]
	logger *zap.SugaredLogger

	// verdictCursor is the last verdict event id the current pass examined.
	verdictCursor int64

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (w *pruner) start(context.Context) error {
	if !w.cfg.enabled() {
		w.logger.Info("every retention is 0; nothing to prune")
		return nil
	}
	if err := w.cfg.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.wg.Add(1)
	go w.loop(ctx)
	return nil
}

func (w *pruner) stop(context.Context) error {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
	return nil
}

func (w *pruner) loop(ctx context.Context) {
	defer w.wg.Done()
	// Offset the first cycle so it does not compete with startup.
	wait := time.NewTimer(2 * time.Minute)
	defer wait.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wait.C:
		}
		pool, err := w.pool.Get()
		if err != nil {
			w.logger.Warnw("acquire pool", "error", err)
		} else {
			w.runCycle(ctx, pool, time.Now())
		}
		wait.Reset(w.cfg.Interval)
	}
}

// runCycle prunes each enabled table. One table's failure does not stop the others.
func (w *pruner) runCycle(ctx context.Context, pool *pgxpool.Pool, now time.Time) {
	for _, step := range []struct {
		table string
		keep  time.Duration
		prune func(context.Context, *pgxpool.Pool, time.Time) (int64, bool, error)
	}{
		{"processor_feedback_events", w.cfg.FeedbackRetention, w.pruneFeedback},
		{"label_evidence", w.cfg.QBStateRetention, w.pruneQBState},
		{"torrent_verdict_events", w.cfg.VerdictEventRetention, w.pruneVerdictEvents},
	} {
		if step.keep <= 0 {
			continue
		}
		var total int64
		for range maxBatches {
			deleted, more, err := step.prune(ctx, pool, now.Add(-step.keep))
			total += deleted
			if err != nil {
				if ctx.Err() == nil {
					w.logger.Warnw("prune failed", "table", step.table, "error", err)
				}
				break
			}
			if !more {
				break
			}
		}
		if total > 0 {
			w.logger.Infow("pruned", "table", step.table, "rows", total)
		}
	}
}

// pruneFeedback deletes the oldest feedback events once they pass the cutoff.
// seq follows insert order, so only the head of the primary key is read: a
// created_at filter alone would scan the whole table when nothing is old.
func (w *pruner) pruneFeedback(ctx context.Context, pool *pgxpool.Pool, cutoff time.Time) (int64, bool, error) {
	tag, err := pool.Exec(ctx, `DELETE FROM processor_feedback_events
WHERE seq IN (SELECT seq FROM processor_feedback_events ORDER BY seq LIMIT $2) AND created_at < $1`, cutoff, w.cfg.BatchSize)
	if err != nil {
		return 0, false, err
	}
	return tag.RowsAffected(), tag.RowsAffected() == int64(w.cfg.BatchSize), nil
}

// pruneQBState deletes old qBittorrent state observations that a newer one
// for the same torrent has replaced. The newest row per torrent always stays,
// because retention and capture treat "has evidence" as a protection. A row in
// a private category is a lasting privacy fact, so only a newer private row
// may replace it.
func (w *pruner) pruneQBState(ctx context.Context, pool *pgxpool.Pool, cutoff time.Time) (int64, bool, error) {
	private := func(alias string) string {
		return "coalesce(" + catalogueguard.QBPrivacySQL(alias+".source", alias+".category", "$3") + ",false)"
	}
	tag, err := pool.Exec(ctx, `DELETE FROM label_evidence WHERE id IN (
SELECT e.id FROM label_evidence e
WHERE e.source_kind='qb_state_observation' AND e.observed_at < $1
  AND EXISTS (SELECT 1 FROM label_evidence n
    WHERE n.info_hash=e.info_hash AND n.source_kind='qb_state_observation'
      AND (n.observed_at,n.id) > (e.observed_at,e.id)
      AND (`+private("n")+` OR NOT `+private("e")+`))
ORDER BY e.observed_at LIMIT $2)`, cutoff, w.cfg.BatchSize, catalogueguard.TagWhitespace)
	if err != nil {
		return 0, false, err
	}
	return tag.RowsAffected(), tag.RowsAffected() == int64(w.cfg.BatchSize), nil
}

// pruneVerdictEvents examines the next batch of verdict events in id order and
// deletes the old ones a newer event for the same torrent has superseded. The
// newest event per torrent always stays, so torrent_verdict_state can still be
// rebuilt from the events. Operator and CSAM events, and events a recovery
// snapshot points at, are kept as the audit trail. Kept rows stay at the head
// of the table, so the pass walks by id instead of rescanning them each time.
// Ids follow insert time: the pass ends at the first batch that reaches a row
// inside the retention window.
func (w *pruner) pruneVerdictEvents(ctx context.Context, pool *pgxpool.Pool, cutoff time.Time) (int64, bool, error) {
	var deleted, scanned int64
	var last *int64
	var reachedCutoff bool
	err := pool.QueryRow(ctx, `WITH scan AS (
  SELECT id,info_hash,mechanism,created_at FROM torrent_verdict_events WHERE id > $1 ORDER BY id LIMIT $2
), gone AS (
  DELETE FROM torrent_verdict_events e USING scan s
  WHERE e.id=s.id AND s.created_at < $3 AND s.mechanism NOT IN ('operator','csam')
    AND EXISTS (SELECT 1 FROM torrent_verdict_events n WHERE n.info_hash=s.info_hash AND n.id > s.id)
    AND s.id NOT IN (SELECT removed_verdict_event_id FROM catalogue_recovery_snapshots WHERE removed_verdict_event_id IS NOT NULL
      UNION ALL SELECT restored_verdict_event_id FROM catalogue_recovery_snapshots WHERE restored_verdict_event_id IS NOT NULL)
  RETURNING 1
)
SELECT (SELECT count(*) FROM gone),(SELECT count(*) FROM scan),(SELECT max(id) FROM scan),
  (SELECT coalesce(bool_or(created_at >= $3),false) FROM scan)`, w.verdictCursor, w.cfg.BatchSize, cutoff).Scan(&deleted, &scanned, &last, &reachedCutoff)
	if err != nil {
		return 0, false, err
	}
	if last == nil || scanned < int64(w.cfg.BatchSize) || reachedCutoff {
		w.verdictCursor = 0 // pass complete; the next cycle starts over
		return deleted, false, nil
	}
	w.verdictCursor = *last
	return deleted, true, nil
}
