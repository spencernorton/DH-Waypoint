package restapifx

import (
	"github.com/spencernorton/DH-Waypoint/internal/database/dhtms"
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/httpserver"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/leanmode"
	"github.com/spencernorton/DH-Waypoint/internal/restapi"
	"github.com/spencernorton/DH-Waypoint/internal/serving"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type apiParams struct {
	fx.In
	Search        lazy.Lazy[search.ServingSearch]
	Logger        *zap.SugaredLogger
	DHTMSClient   *dhtms.Client   `optional:"true"`
	ServingPolicy *serving.Policy `optional:"true"`
	DHTMSMetrics  *dhtms.Metrics  `optional:"true"`
	LeanStore     *leanmode.Store `optional:"true"`
}

func New() fx.Option {
	return fx.Module("rest_api",
		fx.Provide(
			func(p apiParams) *restapi.API {
				return restapi.New(p.Search, p.Logger).WithDHTMS(
					p.DHTMSClient,
					p.ServingPolicy,
					p.DHTMSMetrics,
					p.LeanStore,
				)
			},
			fx.Annotate(func(a *restapi.API) httpserver.Option { return a }, fx.ResultTags(`group:"http_server_options"`)),
		))
}
