// Package llmstagefx wires the LLM fallback stage into the classifier
// runner chain. The stage is installed as an fx.Decorate that wraps
// whatever Runner the classifier module (and canonical-preempt
// decorator) has already produced, so the effective call order is:
//
//	canonical preempt -> LLM stage -> CEL runner
//
// In shadow mode the LLM runs and emits metrics but returns the CEL
// result unchanged. In live mode a type-only callback runs at the workflow's
// explicit policy boundary; accepted types continue through the exclusion tail.
// Runners without that boundary remain shadow-only. Both flags default false so
// including the module is safe; see llmstage.Config for operator
// activation.
package llmstagefx

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/classifier"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/llmmatch"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/llmstage"
	"github.com/spencernorton/DH-Waypoint/internal/config/configfx"
	"github.com/spencernorton/DH-Waypoint/internal/evidence"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/llmcapture"
	"github.com/spencernorton/DH-Waypoint/internal/llmwork"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// llmRunnerWrapper is the named-type fx provides so this module's
// wrapper composes cleanly with classifierfx's canonical wrapper.
//
// We can't use fx.Decorate at module scope (sibling modules see the
// undecorated value — see classifierfx for the same bug). Instead the
// pattern is: classifierfx provides the `lazy.Lazy[Runner]` already
// wrapped with canonical preempt; this module re-Provides under a
// dedicated tag so fx can resolve a precise build order, then a
// `fx.Decorate` at the parent (appfx) level swaps the unwrapped Runner
// for the LLM-wrapped one. The parent-scoped Decorate is the part that
// guarantees every consumer (incl. processorfx) sees the wrapped value.
//
// Implementation here just exposes a constructor for the wrapped lazy;
// appfx wires it via `fx.Decorate` at its level.
func WrapWithLLMStage(
	inner lazy.Lazy[classifier.Runner],
	cfg llmstage.Config,
	store *evidence.Store,
	metrics *llmstage.Metrics,
	logger *zap.SugaredLogger,
	capture llmcapture.Capturer,
	pool lazy.Lazy[*pgxpool.Pool],
	policy classifier.Config,
	optional struct {
		fx.In
		NamePolicy *namepolicy.Policy         `optional:"true"`
		Matcher    *llmmatch.Client           `optional:"true"`
		Work       *llmwork.Store             `optional:"true"`
		Dispatch   llmcapture.DispatchControl `optional:"true"`
	},
) lazy.Lazy[classifier.Runner] {
	return lazy.New(func() (classifier.Runner, error) {
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
		r, err := inner.Get()
		if err != nil {
			return nil, err
		}
		stage := llmstage.NewStage(cfg, r, store, metrics, logger, llmstage.Admission{
			Budget: llmmatch.NewPostgresTypeCallBudget(pool), Capture: capture, Dispatch: optional.Dispatch,
		})
		stage.SetWork(optional.Work, policy)
		stage.SetNamePolicy(optional.NamePolicy)
		stage.SetMatcherWork(optional.Matcher)
		return stage, nil
	})
}

func New() fx.Option {
	return fx.Module(
		"classifier_llm",
		configfx.NewConfigModule[llmstage.Config]("classifier_llm", llmstage.NewDefaultConfig()),
		fx.Provide(llmstage.NewMetrics),
		fx.Provide(fx.Annotated{
			Group: "prometheus_collectors,flatten",
			Target: func(m *llmstage.Metrics) []prometheus.Collector {
				return m.Collectors()
			},
		}),
	)
}
