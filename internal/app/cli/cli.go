package cli

import (
	"context"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/spencernorton/DH-Waypoint/internal/version"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

type Params struct {
	fx.In
	Args       []string `name:"cli_args"`
	Lifecycle  fx.Lifecycle
	Shutdowner fx.Shutdowner
	Commands   []*cli.Command `group:"commands"`
	Logger     *zap.SugaredLogger
	// Depending on the attached app hooks appends them to the lifecycle first,
	// so they stop last: the database pool and DHT server outlive the command.
	Hooks AttachedHooks `optional:"true"`
}

type Result struct {
	fx.Out
	App *cli.App
}

func New(p Params) (Result, error) {
	commands := p.Commands
	sort.Slice(commands, func(i, j int) bool {
		return strings.Compare(commands[i].Name, commands[j].Name) < 0
	})

	name := "dh-waypoint"

	if version.GitTag != "" {
		name += " " + version.GitTag
	}

	app := &cli.App{
		Name:     name,
		Commands: commands,
		CommandNotFound: func(_ *cli.Context, command string) {
			p.Logger.Errorw("command not found", "command", command)
		},
		After: func(*cli.Context) error {
			return p.Shutdowner.Shutdown()
		},
		Version: version.GitTag,
		// disabling the version flag as for some reason the After hook never gets called
		HideVersion: true,
	}

	app.Setup()

	// A stop signal cancels the command context. A long-running command such as
	// "worker run" then returns and its After hook stops the workers.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})

	p.Lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			// todo: Fix!
			//nolint:contextcheck
			go (func() {
				defer close(done)
				// the following hack fixes a weird bug where the CLI does not terminate when calling
				// with just --help
				args := p.Args
				if len(args) == 2 && (args[1] == "-h" || args[1] == "--help") {
					args = []string{args[0]}
				}
				if err := app.RunContext(ctx, args); err != nil {
					panic(err)
				}
			})()
			return nil
		},
		// fx handles the same signal by running the stop hooks and exiting, so
		// wait here for the command to finish its own cleanup first.
		OnStop: func(stopCtx context.Context) error {
			cancel()
			select {
			case <-done:
				return nil
			case <-stopCtx.Done():
				return stopCtx.Err()
			}
		},
	})

	return Result{
		App: app,
	}, nil
}
