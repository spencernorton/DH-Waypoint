package restapifx

import (
	"github.com/spencernorton/DH-Waypoint/internal/database/search"
	"github.com/spencernorton/DH-Waypoint/internal/httpserver"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/restapi"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

func New() fx.Option {
	return fx.Module("rest_api",
		fx.Provide(
			func(s lazy.Lazy[search.ServingSearch], l *zap.SugaredLogger) *restapi.API {
				return restapi.New(s, l)
			},
			fx.Annotate(func(a *restapi.API) httpserver.Option { return a }, fx.ResultTags(`group:"http_server_options"`)),
		))
}
