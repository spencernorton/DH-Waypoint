// Package torrentcmd provides `dh-waypoint torrent create`: hash a
// local file or directory into a .torrent file, ready to seed with
// any client and to upload through the private torrent API.
package torrentcmd

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/spencernorton/DH-Waypoint/internal/tracker"
	"github.com/urfave/cli/v2"
	"go.uber.org/fx"
)

type Params struct {
	fx.In
}

type Result struct {
	fx.Out
	Command *cli.Command `group:"commands"`
}

func New(_ Params) (Result, error) {
	cmd := &cli.Command{
		Name:  "torrent",
		Usage: "Create .torrent files from local data",
		Subcommands: []*cli.Command{
			{
				Name:      "create",
				Usage:     "Hash a file or directory into a .torrent file",
				ArgsUsage: "<path>",
				Flags: []cli.Flag{
					&cli.StringSliceFlag{Name: "tracker", Usage: "announce URL to embed (repeatable, first is primary)"},
					&cli.BoolFlag{Name: "private", Usage: "set the BEP-27 private flag"},
					&cli.StringFlag{Name: "source", Usage: "BEP-27 source field (differentiates identical content across private sources)"},
					&cli.Int64Flag{Name: "piece-length", Usage: "piece length in bytes (0 = automatic by size)"},
					&cli.StringFlag{Name: "comment", Usage: "comment to embed"},
					&cli.StringFlag{Name: "output", Aliases: []string{"o"}, Usage: "output .torrent path (default <name>.torrent beside the input)"},
				},
				Action: func(ctx *cli.Context) error {
					if ctx.NArg() != 1 {
						return cli.Exit("usage: dh-waypoint torrent create [flags] <path>", 1)
					}
					root := ctx.Args().First()
					raw, hash, err := tracker.CreateTorrentFile(root, tracker.CreateOptions{
						Trackers:    ctx.StringSlice("tracker"),
						Private:     ctx.Bool("private"),
						Source:      ctx.String("source"),
						PieceLength: ctx.Int64("piece-length"),
						Comment:     ctx.String("comment"),
					})
					if err != nil {
						return cli.Exit(err.Error(), 1)
					}
					out := ctx.String("output")
					if out == "" {
						out = filepath.Join(filepath.Dir(root), filepath.Base(root)+".torrent")
					}
					if err := os.WriteFile(out, raw, 0o644); err != nil {
						return err
					}
					fmt.Fprintf(ctx.App.Writer, "wrote %s\ninfo_hash: %s\n", out, hash.String())
					if trackers := ctx.StringSlice("tracker"); len(trackers) > 0 {
						fmt.Fprintf(ctx.App.Writer, "magnet:?xt=urn:btih:%s&dn=%s&tr=%s\n",
							hash.String(), url.QueryEscape(filepath.Base(root)), url.QueryEscape(trackers[0]))
					}
					if ctx.Bool("private") {
						fmt.Fprintln(ctx.App.Writer, "private: seed this exact .torrent file; upload it with POST /api/private/v1/torrents so members can download a personalised copy.")
					}
					return nil
				},
			},
		},
	}
	return Result{Command: cmd}, nil
}
