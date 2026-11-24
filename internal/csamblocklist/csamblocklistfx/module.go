// Package csamblocklistfx wires the CSAM blocklist into the app.
//
// What it provides:
//
//   - configfx section "csam_blocklist" (env prefix CSAM_BLOCKLIST_*)
//   - csamblocklist.Manager interface (consumed by the dhtcrawler
//     pre-fetch hook in infohash_triage, and by the processor to remove a
//     stored banned-keyword match)
//   - csamblocklist.Exporter interface (consumed by the processor
//     when CEL emits ErrDeleteTorrent)
//   - *csamblocklist.Metrics
//   - prometheus_collectors fx-group entries for the metrics
//   - a worker entry that runs the periodic-refresh poll loop and the
//     one-time sweep of rows stored before the banned-keyword check
//   - configfx section "csam_sweep" (CSAM_SWEEP_MODE) for that sweep
//
// Conservative defaults (see csamblocklist.NewDefaultConfig):
//
//   - Enabled=true with no FeedUrls configured → no feed is fetched until
//     the operator opts into one. The Manager is live either way: it
//     remembers this instance's own banned-keyword matches
//   - ExportEnabled=true writing to a local JSONL log
//   - ExportUpstreamURL="" → no outbound POST until operator opts in
package csamblocklistfx

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/config/configfx"
	"github.com/spencernorton/DH-Waypoint/internal/csamblocklist"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// initialRefreshBudget is how long the OnStart hook is willing to
// wait for the first feed-refresh to complete before letting the app
// boot anyway. Bounded so a misconfigured / unreachable feed cannot
// permanently block startup. The CSAM blocklist filter stays empty until
// the first refresh succeeds — but the dhtcrawler also won't have
// accepted any DHT traffic before fx finishes its OnStart phase, so
// the bounded wait closes the "fresh-deploy gap" Jeeves flagged on
const initialRefreshBudget = 30 * time.Second

// New returns the fx Option that wires csamblocklist into the app.
func New() fx.Option {
	return fx.Module(
		"csamblocklist",
		configfx.NewConfigModule[csamblocklist.Config]("csam_blocklist", csamblocklist.NewDefaultConfig()),
		// Its own section: CSAM_BLOCKLIST_ENABLED switches the outside feeds
		// only, and the sweep does not depend on it.
		configfx.NewConfigModule[csamblocklist.SweepConfig]("csam_sweep", csamblocklist.NewDefaultSweepConfig()),
		fx.Provide(
			csamblocklist.NewMetrics,
			provideManager,
			provideExporter,
		),
		fx.Provide(fx.Annotated{
			Group: "prometheus_collectors,flatten",
			Target: func(m *csamblocklist.Metrics) []prometheus.Collector {
				return m.Collectors()
			},
		}),
		fx.Provide(fx.Annotated{
			Group:  "workers",
			Target: provideRefreshWorker,
		}),
		fx.Invoke(registerExporterShutdown),
	)
}

func provideManager(
	cfg csamblocklist.Config,
	metrics *csamblocklist.Metrics,
	logger *zap.SugaredLogger,
	db lazy.Lazy[*gorm.DB],
	pool lazy.Lazy[*pgxpool.Pool],
	exporter csamblocklist.Exporter,
) csamblocklist.Manager {
	// Resolving the gorm handle first runs the migrations, so the
	// observations table exists before the startup refresh reads it.
	migrated := lazy.New(func() (*pgxpool.Pool, error) {
		if _, err := db.Get(); err != nil {
			return nil, err
		}
		return pool.Get()
	})
	return csamblocklist.New(cfg, logger.Named("csamblocklist"), metrics, migrated, exporter)
}

func provideExporter(
	cfg csamblocklist.Config,
	metrics *csamblocklist.Metrics,
	logger *zap.SugaredLogger,
) csamblocklist.Exporter {
	return csamblocklist.NewExporter(cfg, logger.Named("csam-export"), metrics)
}

// provideRefreshWorker wires the periodic Refresh loop into the
// worker manager. NoOp Manager has no Run method; type assertion
// short-circuits cleanly.
// runStartupRefresh runs the synchronous initial refresh that closes the
// cold-start gap before DHT crawling accepts traffic.
//
// External HTTP feeds remain bounded and fail-open so a slow upstream feed
// cannot brick startup, while a database-backed local observations store
// fails closed when csam_local_observations cannot be loaded on boot.
func runStartupRefresh(ctx context.Context, mgr csamblocklist.Manager, log *zap.SugaredLogger, budget time.Duration) error {
	hasLocal := false
	if ls, ok := mgr.(interface{ HasLocalStore() bool }); ok {
		hasLocal = ls.HasLocalStore()
	}
	if !mgr.Enabled() && !hasLocal {
		return nil
	}
	refreshCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if err := mgr.Refresh(refreshCtx); err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return ctx.Err()
		}
		if hasLocal && (errors.Is(err, csamblocklist.ErrLocalObservationsLoad) || errors.Is(err, context.DeadlineExceeded)) {
			log.Errorw(
				"initial CSAM local observations load failed; aborting startup fail-closed",
				"budget", budget.String(),
				"err", err,
			)
			return err
		}
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			log.Warnw(
				"initial CSAM blocklist feed refresh timed out; continuing",
				"budget", budget.String(),
			)
		case errors.Is(err, context.Canceled):
			return nil
		default:
			log.Warnw(
				"initial CSAM blocklist feed refresh had errors; continuing",
				"err", err,
			)
		}
	}
	return nil
}

func provideRefreshWorker(mgr csamblocklist.Manager, sweep csamblocklist.SweepConfig, logger *zap.SugaredLogger) worker.Worker {
	var (
		runCtx    context.Context
		runCancel context.CancelFunc
	)
	log := logger.Named("csamblocklist-worker")
	return worker.NewWorker(
		"csamblocklist",
		fx.Hook{
			OnStart: func(startCtx context.Context) error {
				// Block startup on the first refresh so the dhtcrawler
				// never accepts traffic with an empty bloom filter
				// (review finding: "Initial blocklist refresh does
				// not block crawler startup").
				if err := runStartupRefresh(startCtx, mgr, log, initialRefreshBudget); err != nil {
					return err
				}

				// Periodic-refresh loop runs in the background for
				// the remainder of app lifetime.
				runCtx, runCancel = context.WithCancel(context.Background())
				if runner, ok := mgr.(interface{ Run(context.Context) }); ok {
					go runner.Run(runCtx)
				}
				if sweeper, ok := mgr.(csamblocklist.Sweeper); ok {
					go sweeper.RunSweep(runCtx, sweep.Mode)
				}
				return nil
			},
			OnStop: func(_ context.Context) error {
				if runCancel != nil {
					runCancel()
				}
				return nil
			},
		},
	)
}

// registerExporterShutdown closes the JSONL file handle on app stop.
func registerExporterShutdown(lc fx.Lifecycle, exp csamblocklist.Exporter) {
	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			return exp.Close()
		},
	})
}
