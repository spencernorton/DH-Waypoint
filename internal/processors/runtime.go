package processors

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// Worker never makes optional availability a core startup/readiness condition.
func (s *Store) Worker() worker.Worker {
	var cancel context.CancelFunc
	var wg sync.WaitGroup
	return worker.NewWorker("processors", fx.Hook{OnStart: func(context.Context) error {
		ctx, c := context.WithCancel(context.Background())
		cancel = c
		launch := func(interval time.Duration, f func(context.Context)) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tick := time.NewTicker(interval)
				defer tick.Stop()
				for {
					f(ctx)
					select {
					case <-ctx.Done():
						return
					case <-tick.C:
					}
				}
			}()
		}
		launch(s.cfg.PollInterval, func(ctx context.Context) {
			if _, err := s.Scan(ctx); err != nil {
				s.metrics.admission.WithLabelValues("unavailable").Inc()
			}
		})
		launch(30*time.Second, func(ctx context.Context) {
			_ = s.Refresh(ctx)
			if err := s.SyncPluginDir(ctx); err != nil {
				s.log().Warnw("plugin directory sync", "error", err)
			}
			_ = s.Refresh(ctx)
			_, _ = s.DeliverFeedback(ctx)
			s.updateGauges(ctx)
		})
		for range s.cfg.Concurrency {
			var failures stepFailures
			launch(s.cfg.PollInterval, func(ctx context.Context) {
				if err := drain(ctx, s.RunOne); err != nil {
					failures.note(s.log(), err, time.Now())
				}
			})
		}
		return nil
	}, OnStop: func(ctx context.Context) error {
		if cancel == nil {
			return nil
		}
		cancel()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
}

// drain runs ready steps back to back while they succeed, so a full queue is
// not paced to one step per poll tick. A stale lease (its inputs or the core
// policy changed, as for jobs queued before a release) was already settled, so
// draining continues. An empty queue or any other error returns to the
// caller's ticker, which keeps failing work at one attempt per tick.
func drain(ctx context.Context, run func(context.Context) (bool, error)) error {
	for ctx.Err() == nil {
		worked, err := run(ctx)
		if worked && errors.Is(err, ErrStale) {
			continue
		}
		if err != nil || !worked {
			return err
		}
	}
	return nil
}

// stepFailures reports at most one failed step per minute with the count since
// the last report, so a stalled queue is visible without flooding the log.
type stepFailures struct {
	last  time.Time
	count int
}

func (f *stepFailures) note(log *zap.SugaredLogger, err error, now time.Time) {
	f.count++
	if !f.last.IsZero() && now.Sub(f.last) < time.Minute {
		return
	}
	log.Warnw("processor step failed", "error", err, "failures", f.count)
	f.last, f.count = now, 0
}
