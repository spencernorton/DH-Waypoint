package tmdbhealthcheck

import (
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/health"
	"github.com/spencernorton/DH-Waypoint/internal/lazy"
	"github.com/spencernorton/DH-Waypoint/internal/tmdb"
	"go.uber.org/fx"
)

type Params struct {
	fx.In
	Config tmdb.Config
	Client lazy.Lazy[tmdb.Client]
}

type Result struct {
	fx.Out
	Option health.CheckerOption `group:"health_check_options"`
}

func New(p Params) Result {
	return Result{
		Option: health.WithPeriodicCheck(
			time.Minute*5,
			0,
			// Without a key TMDB is not configured, not down: it must not fail /status.
			NewCheck(p.Config.Enabled && p.Config.APIKey != "", p.Client),
		),
	}
}
