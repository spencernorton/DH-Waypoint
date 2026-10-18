package namepolicyfx

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/config/configfx"
	"github.com/spencernorton/DH-Waypoint/internal/httpserver"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy"
	policyhttp "github.com/spencernorton/DH-Waypoint/internal/namepolicy/httpserver"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module("name_policy",
		configfx.NewConfigModule[namepolicy.Config]("name_policy", namepolicy.NewDefaultConfig()),
		fx.Provide(namepolicy.New),
		fx.Provide(fx.Annotated{Group: "prometheus_collectors,flatten", Target: func(p *namepolicy.Policy) []prometheus.Collector { return p.Collectors() }}),
		fx.Provide(fx.Annotated{Group: "http_server_options", Target: func(p *namepolicy.Policy) httpserver.Option { return policyhttp.New(p) }}),
	)
}
