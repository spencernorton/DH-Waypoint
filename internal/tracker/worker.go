package tracker

import (
	"context"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/worker"
	"go.uber.org/fx"
)

// Worker periodically sweeps expired tracker peer rows. It runs
// only when the tracker is configured on; otherwise the hook exits
// immediately and the worker costs nothing.
func (s *Store) Worker() worker.Worker {
	var cancel context.CancelFunc
	done := make(chan struct{})
	return worker.NewWorker("tracker", fx.Hook{
		OnStart: func(context.Context) error {
			ctx, c := context.WithCancel(context.Background())
			cancel = c
			go func() {
				defer close(done)
				if !s.cfg.TrackerActive() {
					return
				}
				tick := time.NewTicker(time.Hour)
				defer tick.Stop()
				for {
					if _, err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
						s.log().Warnw("tracker peer sweep", "error", err)
					}
					select {
					case <-ctx.Done():
						return
					case <-tick.C:
					}
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			if cancel != nil {
				cancel()
			}
			select {
			case <-done:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
}
