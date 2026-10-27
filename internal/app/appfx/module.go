package appfx

import (
	"github.com/spencernorton/DH-Waypoint/internal/animedb/animedbfx"
	"github.com/spencernorton/DH-Waypoint/internal/app/cli"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/animebackfillcmd"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/attributioncmd"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/backloglocalclassifycmd"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/classifiercmd"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/configcmd"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/episodesbackfillcmd"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/granularitybackfillcmd"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/plugincmd"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/policyshadowcmd"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/processcmd"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/purgecontenttypescmd"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/refreshalttitlescmd"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/refreshanimetitlescmd"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/refreshseedscmd"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/releasefieldsrepaircmd"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/reprocesscmd"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/setupcmd"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/torrentcmd"
	"github.com/spencernorton/DH-Waypoint/internal/app/cmd/workercmd"
	"github.com/spencernorton/DH-Waypoint/internal/attribution"
	"github.com/spencernorton/DH-Waypoint/internal/blocking"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/classifierfx"
	"github.com/spencernorton/DH-Waypoint/internal/classifier/contentfilter/contentfilterfx"
	"github.com/spencernorton/DH-Waypoint/internal/config/configfx"
	"github.com/spencernorton/DH-Waypoint/internal/csamblocklist/csamblocklistfx"
	"github.com/spencernorton/DH-Waypoint/internal/dashstats/dashstatsfx"
	"github.com/spencernorton/DH-Waypoint/internal/database/databasefx"
	"github.com/spencernorton/DH-Waypoint/internal/database/migrations"
	"github.com/spencernorton/DH-Waypoint/internal/dhtcrawler/dhtcrawlerfx"
	"github.com/spencernorton/DH-Waypoint/internal/evidence/evidencefx"
	"github.com/spencernorton/DH-Waypoint/internal/gql/gqlfx"
	"github.com/spencernorton/DH-Waypoint/internal/health"
	"github.com/spencernorton/DH-Waypoint/internal/httpserver/httpserverfx"
	"github.com/spencernorton/DH-Waypoint/internal/importer/importerfx"
	"github.com/spencernorton/DH-Waypoint/internal/junkpurge/junkpurgefx"
	"github.com/spencernorton/DH-Waypoint/internal/logging/loggingfx"
	"github.com/spencernorton/DH-Waypoint/internal/metrics/queuemetrics"
	"github.com/spencernorton/DH-Waypoint/internal/metrics/torrentmetrics"
	"github.com/spencernorton/DH-Waypoint/internal/namepolicy/namepolicyfx"
	"github.com/spencernorton/DH-Waypoint/internal/processor/processorfx"
	"github.com/spencernorton/DH-Waypoint/internal/processors/processorsfx"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/dht/dhtfx"
	"github.com/spencernorton/DH-Waypoint/internal/protocol/metainfo/metainfofx"
	"github.com/spencernorton/DH-Waypoint/internal/queue/queuefx"
	"github.com/spencernorton/DH-Waypoint/internal/restapi/restapifx"
	"github.com/spencernorton/DH-Waypoint/internal/retention/retentionfx"
	"github.com/spencernorton/DH-Waypoint/internal/seeds/seedsfx"
	telemetryhttp "github.com/spencernorton/DH-Waypoint/internal/telemetry/httpserver"
	telemetryprom "github.com/spencernorton/DH-Waypoint/internal/telemetry/prometheus"
	"github.com/spencernorton/DH-Waypoint/internal/tmdb/tmdbfx"
	"github.com/spencernorton/DH-Waypoint/internal/torznab/torznabfx"
	"github.com/spencernorton/DH-Waypoint/internal/tracker/trackerfx"
	"github.com/spencernorton/DH-Waypoint/internal/validation"
	"github.com/spencernorton/DH-Waypoint/internal/verdicts/verdictsfx"
	"github.com/spencernorton/DH-Waypoint/internal/version"
	"github.com/spencernorton/DH-Waypoint/internal/version/healthcheck"
	"github.com/spencernorton/DH-Waypoint/internal/wantbridge/wantbridgefx"
	"github.com/spencernorton/DH-Waypoint/internal/worker"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"app",
		animedbfx.New(),
		fx.Module("blocking", fx.Provide(blocking.New)),
		classifierfx.New(),
		contentfilterfx.New(),
		configfx.New(),
		csamblocklistfx.New(),
		dhtcrawlerfx.New(),
		dhtfx.New(),
		databasefx.New(),
		dashstatsfx.New(),
		evidencefx.New(),
		gqlfx.New(),
		fx.Module("health", fx.Provide(health.New)),
		httpserverfx.New(),
		importerfx.New(),
		loggingfx.New(),
		metainfofx.New(),
		fx.Module("queue", fx.Provide(queuemetrics.New, torrentmetrics.New)),
		namepolicyfx.New(),
		processorfx.New(),
		processorsfx.New(),
		queuefx.New(),
		junkpurgefx.New(),
		verdictsfx.New(),
		retentionfx.New(),
		seedsfx.New(),
		fx.Module("telemetry", fx.Provide(telemetryhttp.New, telemetryprom.New)),
		tmdbfx.New(),
		torznabfx.New(),
		trackerfx.New(),
		restapifx.New(),
		fx.Module("validation", fx.Provide(validation.New)),
		fx.Module(
			"version",
			fx.Provide(fx.Annotated{
				Name: "app_version",
				Target: func() string {
					v := version.GitTag
					if v == "" {
						v = "unknown"
					}
					return v
				},
			}),
			fx.Provide(healthcheck.New),
		),
		wantbridgefx.New(),
		fx.Module("worker", fx.Provide(worker.NewRegistry)),
		// Attribution config — registered here rather than its own
		// fx module because attribution doesn't need a worker /
		// background loop yet (one-shot CLI only). When that
		// changes, promote to attributionfx.New().
		configfx.NewConfigModule[attribution.Config]("attribution", attribution.NewDefaultConfig()),
		fx.Provide(
			cli.NewArgs,
			cli.New,
			cli.AttachHooks,
			// cli commands:
			animebackfillcmd.New,
			attributioncmd.New,
			backloglocalclassifycmd.New,
			classifiercmd.New,
			episodesbackfillcmd.New,
			releasefieldsrepaircmd.New,
			granularitybackfillcmd.New,
			configcmd.New,
			purgecontenttypescmd.New,
			refreshalttitlescmd.New,
			refreshanimetitlescmd.New,
			refreshseedscmd.New,
			reprocesscmd.New,
			plugincmd.New,
			processcmd.New,
			setupcmd.New,
			torrentcmd.New,
			policyshadowcmd.New,
			workercmd.New,
		),
		fx.Decorate(migrations.NewDecorator),
	)
}
