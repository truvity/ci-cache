// Command ci-cache is the cache: the server, the toolchain agent that talks
// to it, and the administrative client that empties it.
//
// One binary rather than three because they are one program seen from three
// places -- a server is a disk over a bucket, an agent is a disk over the
// network, `direct` is a disk over a bucket again -- and three binaries would
// be three chances for those to drift apart.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/urfave/cli/v3"
)

// version is stamped in at link time by goreleaser. The fallback is what a
// developer's own `go build` produces, and saying "dev" is better than saying
// a version number that was never released.
var version = "dev"

func main() {
	cmd := &cli.Command{
		Name:                  "ci-cache",
		Usage:                 "a shared build cache for CI",
		Version:               version,
		EnableShellCompletion: true,
		Commands: []*cli.Command{
			serveCommand(),
			agentCommand(),
			directCommand(),
			adminCommand(),
			versionCommand(),
		},
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		// An ExitCoder has already been printed and exited by urfave; only
		// an ordinary error reaches this point.
		fmt.Fprintf(os.Stderr, "ci-cache: %v\n", err)
		os.Exit(1)
	}
}

func versionCommand() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "print the version this binary was built as",
		Action: func(_ context.Context, cmd *cli.Command) error {
			_, _ = fmt.Fprintln(cmd.Root().Writer, version)
			return nil
		},
	}
}

// newLogger builds the structured logger.
//
// JSON, because these lines are read by a log pipeline rather than by a
// person at a terminal, and an unparseable level is not worth refusing to
// start over: the cache being up matters more than the level being exact.
func newLogger(w io.Writer, level string) *slog.Logger {
	lvl := slog.LevelInfo
	if level != "" {
		if err := lvl.UnmarshalText([]byte(level)); err != nil {
			lvl = slog.LevelInfo
		}
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl}))
}
