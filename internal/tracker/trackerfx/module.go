package trackerfx

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spencernorton/DH-Waypoint/internal/config/configfx"
	"github.com/spencernorton/DH-Waypoint/internal/httpserver"
	"github.com/spencernorton/DH-Waypoint/internal/importer"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/tracker"
	"github.com/spencernorton/DH-Waypoint/internal/worker"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type storeParams struct {
	fx.In

	Config   tracker.Config
	Pool     lazy.Lazy[*pgxpool.Pool]
	Importer lazy.Lazy[importer.Importer]
	Logger   *zap.SugaredLogger
}

func New() fx.Option {
	return fx.Module("tracker",
		configfx.NewConfigModule[tracker.Config]("tracker", tracker.NewDefaultConfig()),
		fx.Provide(
			func(p storeParams) (*tracker.Store, error) {
				s, err := tracker.NewStore(p.Config, p.Pool, p.Importer)
				if err != nil {
					return nil, err
				}
				s.SetLogger(p.Logger)
				return s, nil
			},
			fx.Annotate(func(s *tracker.Store) worker.Worker { return s.Worker() }, fx.ResultTags(`group:"workers"`)),
			fx.Annotate(func(s *tracker.Store) httpserver.Option { return s }, fx.ResultTags(`group:"http_server_options"`)),
		))
}
