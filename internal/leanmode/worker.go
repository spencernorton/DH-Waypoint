package leanmode

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const workerKey = "lean_mode"

type WorkerParams struct {
	fx.In
	Config  Config
	Store   *Store
	Metrics *Metrics
	Logger  *zap.SugaredLogger
}

type WorkerResult struct {
	fx.Out
	Worker worker.Worker `group:"workers"`
}

func NewWorker(p WorkerParams) WorkerResult {
	w := &leanWorker{
		cfg:     p.Config,
		store:   p.Store,
		metrics: p.Metrics,
		logger:  p.Logger.Named("lean_mode"),
	}
	return WorkerResult{Worker: worker.NewWorker(workerKey, fx.Hook{
		OnStart: w.start,
		OnStop:  w.stop,
	})}
}

type leanWorker struct {
	cfg     Config
	store   *Store
	metrics *Metrics
	logger  *zap.SugaredLogger

	profile ProfileConfig
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func (w *leanWorker) start(context.Context) error {
	if !w.cfg.Enabled {
		w.logger.Info("lean_mode disabled via config; skipping start")
		return nil
	}
	if w.cfg.ConfigFile == "" {
		return errors.New("lean_mode: config_file is required when enabled")
	}
	if w.cfg.Interval <= 0 {
		return errors.New("lean_mode: interval must be positive when enabled")
	}
	prof, err := LoadProfileFile(w.cfg.ConfigFile)
	if err != nil {
		return fmt.Errorf("lean_mode: load profile: %w", err)
	}
	if !prof.Enabled {
		w.logger.Infow("lean_mode profile has enabled=false; worker remains inert", "profile", prof.ID)
		return nil
	}
	if prof.Mode == ModePrune && !prof.Prune.ConfirmDestructive {
		return ErrDestructiveUnconfirmed
	}
	w.profile = prof
	w.store.SetActiveProfile(&prof, w.cfg.ServeGlobally)

	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.wg.Add(1)
	go w.loop(ctx)
	return nil
}

func (w *leanWorker) stop(context.Context) error {
	w.store.SetActiveProfile(nil, false)
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
	return nil
}

func (w *leanWorker) loop(ctx context.Context) {
	defer w.wg.Done()

	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}

	w.runCycle(ctx)
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.runCycle(ctx)
		}
	}
}

func (w *leanWorker) runCycle(ctx context.Context) {
	start := time.Now()
	report, err := w.store.Reconcile(ctx, w.profile, ReconcileOptions{
		TitleBatchSize: w.cfg.TitleBatchSize,
	})
	w.metrics.cycleDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		w.metrics.cycleErrorsTotal.WithLabelValues("reconcile").Inc()
		w.logger.Warnw("lean_mode cycle failed", "profile", w.profile.ID, "err", err)
		return
	}
	w.metrics.cyclesTotal.WithLabelValues(w.profile.ID, w.profile.Mode).Inc()
	w.metrics.slotsEvaluated.WithLabelValues(w.profile.ID).Add(float64(report.SlotsEvaluated))
	w.metrics.winnersGauge.WithLabelValues(w.profile.ID).Set(float64(report.WinnersKept))
	w.metrics.runnerUpsGauge.WithLabelValues(w.profile.ID).Set(float64(report.RunnerUpsKept))
	w.metrics.wouldPruneTotal.WithLabelValues(w.profile.ID).Add(float64(report.WouldPruneCount))
	w.metrics.prunedTotal.WithLabelValues(w.profile.ID).Add(float64(report.PrunedCount))
	w.metrics.revivedSlotsTotal.WithLabelValues(w.profile.ID).Add(float64(report.RevivedSlots))
	for reason, n := range report.DisqualByReason {
		w.metrics.disqualifiedTotal.WithLabelValues(w.profile.ID, reason).Add(float64(n))
	}
	w.logger.Infow("lean_mode cycle complete",
		"profile", report.ProfileID,
		"mode", report.Mode,
		"titles", report.TitlesEvaluated,
		"slots", report.SlotsEvaluated,
		"winners", report.WinnersKept,
		"runner_ups", report.RunnerUpsKept,
		"would_prune", report.WouldPruneCount,
		"pruned", report.PrunedCount,
		"duration_ms", report.DurationMs,
	)
}
