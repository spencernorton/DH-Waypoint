package configfx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrg/xdg"
	"github.com/go-playground/validator/v10"
	"github.com/spencernorton/DH-Waypoint/internal/config"
	"github.com/spencernorton/DH-Waypoint/internal/config/configresolver"
	"go.uber.org/fx"
)

func New() fx.Option {
	osEnv := ReadOsEnv()

	//nolint:prealloc
	var options []fx.Option

	var extraConfigFiles []string
	if osEnv[extraFilesKey] != "" {
		extraConfigFiles = strings.Split(osEnv[extraFilesKey], ",")
	}

	for i, file := range extraConfigFiles {
		options = append(options,
			fx.Provide(
				fx.Annotated{
					Group: "config_resolvers",
					Target: func(val *validator.Validate) (configresolver.Resolver, error) {
						return configresolver.NewFromYamlFile(
							file,
							false,
							val,
							configresolver.WithPriority(-i),
						)
					},
				},
			))
	}

	options = append(options,
		fx.Provide(config.New),
		fx.Provide(fx.Annotated{
			Group: "config_resolvers",
			Target: func() (configresolver.Resolver, error) {
				return configresolver.NewEnv(
					osEnv,
					configresolver.WithPriority(-len(extraConfigFiles)),
				), nil
			},
		}),
		fx.Provide(
			fx.Annotated{
				Group: "config_resolvers",
				Target: func(val *validator.Validate) (configresolver.Resolver, error) {
					return configresolver.NewFromYamlFile(
						"./config.yml",
						true,
						val,
						configresolver.WithPriority(10),
					)
				},
			},
		),
	)
	configFilePath := filepath.Join(xdg.ConfigHome, "bitmagnet", "config.yml")
	if resolved, err := xdg.ConfigFile("bitmagnet/config.yml"); err == nil {
		configFilePath = resolved
	} else if _, statErr := os.Stat(configFilePath); statErr != nil && !os.IsNotExist(statErr) {
		_, _ = fmt.Fprintf(os.Stderr, "warning: unable to access %s (%v); check volume mount path and UID 1000:1000 ownership\n", configFilePath, statErr)
		configFilePath = ""
	} else if statErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: unable to initialize config directory for %s (%v); falling back to environment variables\n", configFilePath, err)
		configFilePath = ""
	}
	if configFilePath != "" {
		options = append(options,
			fx.Provide(
				fx.Annotated{
					Group: "config_resolvers",
					Target: func(val *validator.Validate) (configresolver.Resolver, error) {
						return configresolver.NewFromYamlFile(
							configFilePath,
							true,
							val,
							configresolver.WithPriority(20),
						)
					},
				},
			),
		)
	}

	return fx.Module(
		"config",
		fx.Options(options...),
	)
}

func ReadOsEnv() map[string]string {
	rawEnv := os.Environ()
	env := make(map[string]string, len(rawEnv))

	for _, rawEnvEntry := range rawEnv {
		parts := strings.SplitN(rawEnvEntry, "=", 2)
		env[parts[0]] = parts[1]
	}

	return env
}

const extraFilesKey = "EXTRA_CONFIG_FILES"
