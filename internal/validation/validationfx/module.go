package validationfx

import (
	"github.com/spencernorton/DH-Waypoint/internal/validation"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"validation",
		fx.Provide(validation.New),
	)
}
