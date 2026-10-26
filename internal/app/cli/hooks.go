package cli

import (
	"go.uber.org/fx"
)

type HooksParams struct {
	fx.In
	Lifecycle fx.Lifecycle
	Hooks     []fx.Hook `group:"app_hooks"`
}

type HooksResult struct {
	fx.Out
	AttachedHooks
}

type AttachedHooks struct{}

func AttachHooks(p HooksParams) (HooksResult, error) {
	for _, hook := range p.Hooks {
		p.Lifecycle.Append(hook)
	}

	return HooksResult{}, nil
}
