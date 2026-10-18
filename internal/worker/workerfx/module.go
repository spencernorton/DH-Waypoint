package workerfx

import (
	"github.com/spencernorton/DH-Waypoint/internal/worker"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"worker",
		fx.Provide(worker.NewRegistry),
	)
}
