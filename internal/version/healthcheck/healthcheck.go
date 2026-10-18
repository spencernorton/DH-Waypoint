package healthcheck

import (
	"github.com/spencernorton/DH-Waypoint/internal/health"
	"github.com/spencernorton/DH-Waypoint/internal/version"
	"go.uber.org/fx"
)

type Result struct {
	fx.Out
	HealthOption health.CheckerOption `group:"health_check_options"`
}

func New() Result {
	return Result{
		HealthOption: health.WithInfo(map[string]any{
			"name":    "bitmagnet",
			"version": version.GitTag,
		}),
	}
}
