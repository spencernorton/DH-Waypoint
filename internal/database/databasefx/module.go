package databasefx

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/classifier"
	"github.com/spencernorton/DH-Waypoint/internal/config/configfx"
	"github.com/spencernorton/DH-Waypoint/internal/database"
	"github.com/spencernorton/DH-Waypoint/internal/database/cache"
	"github.com/spencernorton/DH-Waypoint/internal/database/dao"
	"github.com/spencernorton/DH-Waypoint/internal/database/healthcheck"
	"github.com/spencernorton/DH-Waypoint/internal/database/migrations"
	"github.com/spencernorton/DH-Waypoint/internal/database/pgstats/pgstatsfx"
	"github.com/spencernorton/DH-Waypoint/internal/database/postgres"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	policyhttp "github.com/spencernorton/DH-Waypoint/internal/namepolicy/httpserver"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"database",
		configfx.NewConfigModule[postgres.Config]("postgres", postgres.NewDefaultConfig()),
		configfx.NewConfigModule[cache.Config]("gorm_cache", cache.NewDefaultConfig()),
		configfx.NewConfigModule[serving.Config]("serving", serving.Config{}),
		fx.Provide(
			serving.NewMetrics,
			func(cfg serving.Config, names *namepolicy.Policy) (*serving.Policy, error) {
				strong, media, err := classifier.CoreAdultServingEvidence()
				if err != nil {
					return nil, err
				}
				cfg.ProcessorExclusions = true
				return serving.NewPolicy(cfg, strong, media, names)
			},
			cache.NewInMemoryCacher,
			cache.NewPlugin,
			dao.New,
			database.New,
			healthcheck.New,
			migrations.New,
			postgres.New,
			search.New,
		),
		fx.Provide(fx.Annotated{
			Group:  "http_server_options",
			Target: policyhttp.NewPublicHash,
		}),
		fx.Provide(fx.Annotated{
			Group:  "prometheus_collectors,flatten",
			Target: func(m *serving.Metrics) []prometheus.Collector { return m.Collectors() },
		}),
		fx.Decorate(
			cache.NewDecorator,
		),
		pgstatsfx.New(),
	)
}
