package cli

import (
	"os"

	"go.uber.org/fx"
)

type ArgsResult struct {
	fx.Out
	Args []string `name:"cli_args"`
}

func NewArgs() (ArgsResult, error) {
	return ArgsResult{Args: os.Args}, nil
}
