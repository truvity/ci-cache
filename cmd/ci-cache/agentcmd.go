package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/truvity/ci-cache/agent"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// The agent's own settings are not part of config.Config: they belong to one
// process on one runner rather than to an installation, and putting them in
// the shared schema would mean the chart rendering values that no server ever
// reads. They get the same prefix and the same shape, under an "agent"
// section that exists only here.
const (
	envAgentRemote  = envPrefix + "AGENT_REMOTE"
	envAgentDir     = envPrefix + "AGENT_CACHE_DIR"
	envAgentBudget  = envPrefix + "AGENT_LOCAL_BUDGET"
	envAgentLabel   = envPrefix + "AGENT_LABEL"
	envAgentMetrics = envPrefix + "AGENT_METRICS"

	// envLegacyMetrics is the name go-cache-plugin taught everyone to set. It
	// still works, because a habit that survives a migration is cheaper to
	// keep than to argue with.
	envLegacyMetrics = "GOCACHE_METRICS"
)

func agentCommand() *cli.Command {
	return &cli.Command{
		Name:  "agent",
		Usage: "serve the Go toolchain's build cache from a cache server (GOCACHEPROG)",
		Description: "Run as the program named by GOCACHEPROG. The toolchain speaks its cache\n" +
			"protocol on stdin and stdout, so nothing but the protocol may be written to\n" +
			"stdout: logs and the metrics summary go to stderr.",
		Flags: append(agentFlags(), &cli.StringFlag{
			Name:    "remote",
			Usage:   "cache server base URL, including the front-end path",
			Sources: cli.EnvVars(envAgentRemote),
		}),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runAgent(ctx, cmd, cmd.String("remote"), false)
		},
	}
}

func directCommand() *cli.Command {
	return &cli.Command{
		Name:  "direct",
		Usage: "serve the Go toolchain's build cache straight from the object store (GOCACHEPROG)",
		Description: "The same program as `agent` with no server in the chain: a local disk over\n" +
			"a bucket. It is what a laptop runs, and what a runner falls back to when there\n" +
			"is no cache server to point at.",
		Flags: agentFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return runAgent(ctx, cmd, "", true)
		},
	}
}

// agentFlags is everything both modes take. The object store's own settings
// come from the shared schema, so a bucket is described identically whether
// it is reached by the server or by the runner.
func agentFlags() []cli.Flag {
	return append(configFlags("store"),
		&cli.StringFlag{
			Name:    "cache-dir",
			Usage:   "local cache directory",
			Sources: cli.EnvVars(envAgentDir),
		},
		&cli.Int64Flag{
			Name:    "local-budget",
			Usage:   "bytes the local cache may use; 0 derives one from the filesystem",
			Sources: cli.EnvVars(envAgentBudget),
		},
		&cli.IntFlag{
			Name:    "upload-workers",
			Sources: cli.EnvVars("CI_CACHE_AGENT_UPLOAD_WORKERS"),
			Usage:   "how many objects may be recorded into the chain at once, behind the build; 0 picks a default",
		},
		&cli.StringFlag{
			Name:    "label",
			Usage:   "name this agent in the metrics summary",
			Sources: cli.EnvVars(envAgentLabel),
		},
		&cli.BoolFlag{
			Name:    "metrics",
			Usage:   "print a one-line summary to stderr at exit",
			Sources: cli.EnvVars(envAgentMetrics, envLegacyMetrics),
		},
		&cli.StringFlag{
			Name:    "pprof",
			Usage:   "serve net/http/pprof on this address (e.g. 127.0.0.1:6060); for profiling a build, never for a runner fleet",
			Sources: cli.EnvVars(envPrefix + "AGENT_PPROF"),
		},
		&cli.StringFlag{
			Name:    "log-level",
			Value:   "warn",
			Usage:   "debug, info, warn or error; the agent logs to stderr",
			Sources: cli.EnvVars(envPrefix + "AGENT_LOG_LEVEL"),
		},
	)
}

func runAgent(ctx context.Context, cmd *cli.Command, remote string, direct bool) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return cli.Exit("ci-cache: "+err.Error(), 1)
	}

	// stderr, not stdout. stdout is the toolchain's channel and a log line in
	// it is not noise, it is a protocol error that fails the build.
	log := newLogger(os.Stderr, cmd.String("log-level"))

	opts := agent.Options{
		Remote:        remote,
		CacheDir:      cmd.String("cache-dir"),
		LocalBudget:   cmd.Int64("local-budget"),
		UploadWorkers: cmd.Int("upload-workers"),
		Metrics:       cmd.Bool("metrics"),
		Label:         cmd.String("label"),
		Logf:          agentLogf(log),
	}
	if cfg.Store.Bucket != "" {
		store := cfg.Store
		opts.Bucket = &store
	}
	// `direct` with nothing behind the disk is not a cache at all, and it is
	// the one mode where saying so costs nobody a build: it is run by hand,
	// not by a job that is already waiting.
	if direct && opts.Bucket == nil {
		return cli.Exit("ci-cache: direct mode needs an object store: set --store.bucket or "+
			envPrefix+"STORE_BUCKET", 1)
	}
	// `agent` with nothing behind it is almost always a job that forgot to
	// set the address, but refusing would fail the build over a cache -- so
	// it is said once and the build goes on.
	if !direct && remote == "" && opts.Bucket == nil {
		log.Warn("no cache server and no object store configured: this is a local cache only",
			"hint", "set --remote or "+envAgentRemote)
	}
	if addr := cmd.String("pprof"); addr != "" {
		startPprof(addr, opts.Logf)
	}

	return agent.Run(ctx, opts)
}

// agentLogf adapts the agent's printf sink to structured logging.
//
// The agent marks the lines that matter and leaves everything else as
// running commentary, which is the right default for a program that runs
// inside somebody else's build: at the default level a working cache says
// nothing at all, and a degraded one says exactly why.
func agentLogf(log *slog.Logger) func(string, ...any) {
	return func(format string, args ...any) {
		if msg, ok := strings.CutPrefix(format, agent.WarnPrefix); ok {
			log.Warn(fmt.Sprintf(msg, args...))
			return
		}
		log.Debug(fmt.Sprintf(format, args...))
	}
}

// startPprof serves the standard profiles on addr for the life of the agent.
//
// The agent is a subprocess of `go`, so there is no other way to profile it
// under a real build: it must expose the endpoints itself. Everything goes
// to the agent's log and never to stdout, which is the protocol's channel.
// A listen failure is logged and ignored -- a build must not fail because
// a profiling port was busy.
func startPprof(addr string, logf func(string, ...any)) {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logf("pprof: listen %s: %v (profiling disabled)", addr, err)

		return
	}

	logf("pprof: serving on http://%s/debug/pprof/", ln.Addr())

	go func() {
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		_ = srv.Serve(ln)
	}()
}
