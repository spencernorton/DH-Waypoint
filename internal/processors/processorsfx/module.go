package processorsfx

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/classifier"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/contentfilter"
	"github.com/spencernorton/DH-Waypoint/internal/config/configfx"
	"github.com/spencernorton/DH-Waypoint/internal/evidence"
	"github.com/spencernorton/DH-Waypoint/internal/httpserver"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	"github.com/spencernorton/DH-Waypoint/internal/processingstate"
	"github.com/spencernorton/DH-Waypoint/internal/processor"
	"github.com/spencernorton/DH-Waypoint/internal/processors"
	"github.com/spencernorton/DH-Waypoint/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type storeParams struct {
	fx.In

	Config     processors.Config
	Processing processor.Config
	Pool       lazy.Lazy[*pgxpool.Pool]
	Classifier classifier.Config
	Filter     contentfilter.Config
	NamePolicy *namepolicy.Policy
	Evidence   *evidence.Store `optional:"true"`
	Logger     *zap.SugaredLogger
}

func New() fx.Option {
	return fx.Module("processors", configfx.NewConfigModule[processors.Config]("processors", processors.NewDefaultConfig()), fx.Provide(
		func(p storeParams) (*processors.Store, error) {
			s, err := processors.NewConfiguredStore(p.Config, p.Pool, p.Classifier, p.Filter, p.NamePolicy, p.Processing)
			if err != nil {
				return nil, err
			}
			s.SetLogger(p.Logger)
			if p.Evidence != nil {
				p.Evidence.AddPostInsertHook(s.HandleEvidence)
			}
			return s, nil
		},
		func(s *processors.Store) processingstate.Recorder { return s },
		fx.Annotate(func(s *processors.Store) worker.Worker { return s.Worker() }, fx.ResultTags(`group:"workers"`)),
		fx.Annotate(func(s *processors.Store) httpserver.Option { return s }, fx.ResultTags(`group:"http_server_options"`)),
		fx.Annotate(func(s *processors.Store) []prometheus.Collector { return s.Collectors() }, fx.ResultTags(`group:"prometheus_collectors,flatten"`)),
	))
}
