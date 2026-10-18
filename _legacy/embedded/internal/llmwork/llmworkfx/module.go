// Package llmworkfx wires the disabled-by-default optional model task queue.
package llmworkfx

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/classifier"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/contentfilter"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/llmmatch"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/llmstage"
	"github.com/spencernorton/DH-Waypoint/internal/config/configfx"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/llmcapture"
	"github.com/spencernorton/DH-Waypoint/internal/llmwork"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"github.com/spencernorton/DH-Waypoint/internal/processor"
	"github.com/spencernorton/DH-Waypoint/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

func New() fx.Option {
	return fx.Module("llm_work",
		configfx.NewConfigModule[llmwork.Config]("llm_work", llmwork.NewDefaultConfig()),
		fx.Provide(provideStore),
		fx.Provide(provideHandler),
		fx.Provide(fx.Annotated{Group: "workers", Target: provideWorker}),
		fx.Provide(fx.Annotated{Group: "prometheus_collectors,flatten", Target: func(store *llmwork.Store) []prometheus.Collector { return store.Metrics().Collectors() }}),
	)
}

type handlerParams struct {
	NamePolicy *namepolicy.Policy `optional:"true"`
	fx.In
	Pool       lazy.Lazy[*pgxpool.Pool]
	Search     lazy.Lazy[search.Search]
	Runner     lazy.Lazy[classifier.Runner]
	Filter     *contentfilter.Filter
	Matcher    *llmmatch.Client
	Classifier classifier.Config
	Observer   classifier.MatchDecisionObserver
}

func provideHandler(p handlerParams) llmwork.Handler {
	return &processor.DeferredApplyHandler{NamePolicy: p.NamePolicy, Pool: p.Pool, Search: p.Search, Runner: p.Runner, Filter: p.Filter, Matcher: p.Matcher, Classifier: p.Classifier, Observer: p.Observer}
}

func provideWorker(store *llmwork.Store, handler llmwork.Handler, logger *zap.SugaredLogger) (worker.Worker, error) {
	w, err := llmwork.NewWorker(store, handler, logger)
	if err != nil {
		return nil, err
	}
	return worker.NewWorker("llm_work", fx.Hook{OnStart: w.Start, OnStop: w.Stop}), nil
}

type storeParams struct {
	NamePolicy *namepolicy.Policy `optional:"true"`
	fx.In
	Config     llmwork.Config
	Pool       lazy.Lazy[*pgxpool.Pool]
	Capture    llmcapture.Capturer
	Dispatch   llmcapture.DispatchControl
	Classifier classifier.Config
	Type       llmstage.Config
	Language   contentfilter.Config
	Matcher    llmmatch.Config
}

func provideStore(p storeParams) (*llmwork.Store, error) {
	if p.Config.Enabled {
		if p.Capture == nil || !p.Capture.Enabled() || p.Dispatch == nil || !p.Dispatch.Enabled() {
			return nil, fmt.Errorf("llm_work requires enabled durable capture and dispatch control")
		}
		if p.Config.Accepts(llmwork.Type) && (!p.Type.Enabled || p.Classifier.Workflow != "default") {
			return nil, fmt.Errorf("llm_work classifier_type requires enabled type inference and the default policy adapter")
		}
		if p.Config.Accepts(llmwork.Type) {
			if err := p.Type.Validate(); err != nil {
				return nil, err
			}
		}
		if p.Config.Accepts(llmwork.Language) && (!p.Language.Enabled || !p.Language.LLMEnabled || p.Language.EffectiveLLMAction() != contentfilter.LLMActionReview) {
			return nil, fmt.Errorf("llm_work contentfilter requires enabled review-only language inference")
		}
		if p.Config.Accepts(llmwork.Matcher) && !p.Matcher.Enabled {
			return nil, fmt.Errorf("llm_work matcher requires enabled identity inference")
		}
		if p.Config.Accepts(llmwork.Matcher) {
			if err := p.Matcher.Validate(); err != nil {
				return nil, err
			}
		}
	}
	store, err := llmwork.NewStore(p.Config, p.Pool)
	if err != nil {
		return nil, err
	}
	store.SetDispatch(p.Dispatch)
	store.SetNamePolicy(p.NamePolicy)
	return store, nil
}
