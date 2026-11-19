// Package plugincmd provides `dh-waypoint plugin` — inspect registered
// processor plugins, validate plugin files, and probe/verify processor
// services before registering them. Registration itself is by file
// (drop a *.plugin.yml into the plugins directory) or by the
// authenticated operator API; this command never writes the registry.
package plugincmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spencernorton/DH-Waypoint/internal/processors"
	v1 "github.com/spencernorton/DH-Waypoint/processorapi/v1"
	v2 "github.com/spencernorton/DH-Waypoint/processorapi/v2"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
)

type Params struct {
	fx.In
	Store  *processors.Store
	Config processors.Config
}

type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func syntheticDigest(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])
}

func buildVerificationJob(manifest v1.Manifest, cfgRaw json.RawMessage) v1.Job {
	stage := v1.Classify
	if len(manifest.Capabilities) > 0 {
		stage = manifest.Capabilities[0]
	}
	var caps []v1.Stage
	if manifest.Contract == v2.Contract {
		caps = append([]v1.Stage(nil), manifest.Capabilities...)
	}
	fileCount := int64(1)
	ext := "mkv"
	return v1.Job{
		Binding: v1.Binding{
			Contract:            manifest.Contract,
			JobID:               syntheticDigest("verify-job:" + manifest.Processor.ID),
			InputRevision:       syntheticDigest("verify-input"),
			PipelineRevision:    syntheticDigest("verify-pipeline"),
			ConfigurationDigest: syntheticDigest(string(cfgRaw)),
			Processor:           manifest.Processor,
			Stage:               stage,
		},
		Capabilities:  caps,
		Configuration: cfgRaw,
		Input: v1.Input{
			InfoHash:      "0123456789abcdef0123456789abcdef01234567",
			Name:          "Synthetic.Verification.Sample.2026.1080p.WEB-DL.x265-TEST",
			Size:          1073741824,
			Files:         []v1.File{{Path: "Synthetic.Verification.Sample.2026.1080p.WEB-DL.x265-TEST.mkv", Size: 1073741824}},
			FileCount:     &fileCount,
			Extension:     &ext,
			FilesComplete: true,
			Deterministic: v1.Deterministic{
				ProcessingOutcome: "completed",
				ContentType:       "movie",
				Title:             "Synthetic Verification Sample",
				Year:              2026,
				MatchingOutcome:   "not_attempted",
			},
			Candidates:   []v1.Candidate{},
			PriorResults: []v1.Result{},
		},
		Deadline: time.Now().Add(10 * time.Second).UTC().Format(time.RFC3339Nano),
	}
}

func New(p Params) (Result, error) {
	cmd := &cli.Command{
		Name:  "plugin",
		Usage: "Inspect processor plugins and validate plugin files",
		Subcommands: []*cli.Command{
			{
				Name:  "list",
				Usage: "List registered processor plugins",
				Action: func(ctx *cli.Context) error {
					entries, err := p.Store.Entries(ctx.Context)
					if err != nil {
						return err
					}
					tw := tabwriter.NewWriter(ctx.App.Writer, 2, 4, 2, ' ', 0)
					fmt.Fprintln(tw, "ID\tDRIVER\tMODE\tSTATE\tPRIORITY\tENDPOINT")
					for _, e := range entries {
						driver := e.Driver
						if driver == "" {
							driver = v1.DriverHTTP
						}
						fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", e.ID, driver, e.Mode, e.State, e.Priority, e.Endpoint)
					}
					if err := tw.Flush(); err != nil {
						return err
					}
					fmt.Fprintf(ctx.App.Writer, "%d registered; plugins directory: %s\n", len(entries), pluginsDirLabel(p.Config.PluginsDir))
					return nil
				},
			},
			{
				Name:      "validate",
				Usage:     "Validate a plugin file without registering it",
				ArgsUsage: "<file.plugin.yml>",
				Action: func(ctx *cli.Context) error {
					if ctx.NArg() != 1 {
						return cli.Exit("usage: dh-waypoint plugin validate <file.plugin.yml>", 1)
					}
					spec, err := processors.ParsePluginFile(ctx.Args().First())
					if err != nil {
						return cli.Exit(err.Error(), 1)
					}
					w := spec.Write()
					driver := w.Driver
					if driver == "" {
						driver = v1.DriverHTTP
					}
					fmt.Fprintf(ctx.App.Writer, "ok: %s (driver=%s mode=%s state=%s priority=%d enabled=%v)\n",
						spec.ID, driver, w.Mode, w.State, w.Priority, spec.Enabled)
					return nil
				},
			},
			{
				Name:      "test",
				Usage:     "Probe a processor service manifest and status",
				ArgsUsage: "<endpoint>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "auth-env", Usage: "DH_WAYPOINT_PROCESSOR_TOKEN_* variable naming the bearer token"},
				},
				Action: func(ctx *cli.Context) error {
					if ctx.NArg() != 1 {
						return cli.Exit("usage: dh-waypoint plugin test <endpoint>", 1)
					}
					endpoint := strings.TrimRight(ctx.Args().First(), "/")
					transport := processors.NewTransport(10 * time.Second)
					transport.CredentialDirectory = p.Config.CredentialDirectory
					manifest, status, err := transport.Inspect(ctx.Context, v1.RegistryWrite{
						Endpoint:      endpoint,
						AuthEnv:       ctx.String("auth-env"),
						Mode:          "shadow",
						State:         "active",
						Configuration: json.RawMessage(`{}`),
					})
					if err != nil {
						return cli.Exit(fmt.Sprintf("probe failed: %v", err), 1)
					}
					caps := make([]string, len(manifest.Capabilities))
					for i, c := range manifest.Capabilities {
						caps[i] = string(c)
					}
					fmt.Fprintf(ctx.App.Writer, "ok: %s %s (model=%s policy=%s)\n", manifest.Processor.ID, manifest.Processor.Version, manifest.Processor.ModelVersion, manifest.Processor.PolicyVersion)
					fmt.Fprintf(ctx.App.Writer, "capabilities: %s\nstate: %s\n", strings.Join(caps, ","), status.State)
					return nil
				},
			},
			{
				Name:      "verify",
				Usage:     "Run end-to-end contract conformance verification (manifest, status, and synthetic process round-trip)",
				ArgsUsage: "<endpoint>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "auth-env", Usage: "DH_WAYPOINT_PROCESSOR_TOKEN_* variable naming the bearer token"},
					&cli.StringFlag{Name: "config-json", Value: "{}", Usage: "JSON configuration object validated against manifest schema"},
				},
				Action: func(ctx *cli.Context) error {
					if ctx.NArg() != 1 {
						return cli.Exit("usage: dh-waypoint plugin verify <endpoint>", 1)
					}
					endpoint := strings.TrimRight(ctx.Args().First(), "/")
					authEnv := ctx.String("auth-env")
					cfgRaw := json.RawMessage(ctx.String("config-json"))
					transport := processors.NewTransport(10 * time.Second)
					transport.CredentialDirectory = p.Config.CredentialDirectory
					manifest, status, err := transport.Inspect(ctx.Context, v1.RegistryWrite{
						Endpoint:      endpoint,
						AuthEnv:       authEnv,
						Mode:          "shadow",
						State:         "active",
						Configuration: cfgRaw,
					})
					if err != nil {
						return cli.Exit(fmt.Sprintf("inspect failed: %v", err), 1)
					}
					job := buildVerificationJob(manifest, cfgRaw)
					res, err := transport.Process(ctx.Context, endpoint, authEnv, job)
					if err != nil {
						return cli.Exit(fmt.Sprintf("process conformance failed: %v", err), 1)
					}
					caps := make([]string, len(manifest.Capabilities))
					for i, c := range manifest.Capabilities {
						caps[i] = string(c)
					}
					fmt.Fprintf(ctx.App.Writer, "verified: %s %s (contract=%s state=%s outcome=%s capabilities=%s)\n",
						manifest.Processor.ID, manifest.Processor.Version, manifest.Contract, status.State, res.Outcome, strings.Join(caps, ","))
					return nil
				},
			},
		},
	}
	return Result{Command: cmd}, nil
}

func pluginsDirLabel(dir string) string {
	if dir == "" {
		return "(disabled)"
	}
	return dir
}
