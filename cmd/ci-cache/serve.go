package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/truvity/ci-cache/config"
	"github.com/truvity/ci-cache/engine/bucket"
	"github.com/truvity/ci-cache/engine/chain"
	"github.com/truvity/ci-cache/engine/disk"
	"github.com/truvity/ci-cache/engine/gc"
	"github.com/truvity/ci-cache/engine/tier"
	"github.com/truvity/ci-cache/frontend/gomod"
	"github.com/truvity/ci-cache/server"
)

// bucketProbeTimeout bounds the one request that proves the object store is
// answering. It is short because it is on the path to the listener opening,
// and a pod that is slow to become ready is a pod the orchestrator restarts.
const bucketProbeTimeout = 5 * time.Second

func serveCommand() *cli.Command {
	return &cli.Command{
		Name:   "serve",
		Usage:  "run the cache server",
		Flags:  configFlags(),
		Action: runServe,
	}
}

func runServe(ctx context.Context, cmd *cli.Command) error {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return cli.Exit("ci-cache: "+err.Error(), 1)
	}
	log := newLogger(os.Stdout, cfg.LogLevel)

	// Both of these are refusals rather than warnings, and for the same
	// reason: a cache that starts and then cannot do its job is worse than
	// one that does not start, because the first is discovered by whoever
	// is waiting on a build and the second by whoever deployed it.
	if err := cfg.Validate(); err != nil {
		return cli.Exit(fmt.Sprintf("ci-cache: refusing to start: %v", err), 1)
	}
	if err := checkWritable(cfg.Persistence.Dir); err != nil {
		return cli.Exit(fmt.Sprintf("ci-cache: refusing to start: %v", err), 1)
	}

	d, err := disk.New(cfg.Persistence.Dir)
	if err != nil {
		return cli.Exit(fmt.Sprintf("ci-cache: refusing to start: disk tier: %v", err), 1)
	}

	tiers := []tier.Tier{d}
	var (
		b     *bucket.Bucket
		queue *bucket.Queue
		opts  []chain.Option
	)

	b, err = bucket.New(ctx, cfg.Store)
	if err != nil {
		// Constructing the client failed, which is a credential or an
		// endpoint and not an outage. There is nothing behind the disk now
		// and nothing will fix it without a restart, so it is said loudly
		// and the cache runs on the volume alone.
		log.Warn("object store could not be configured; serving from the disk tier alone",
			"bucket", cfg.Store.Bucket, "endpoint", cfg.Store.Endpoint, "err", err)
	} else {
		// A bucket that is not ANSWERING is a different matter, and it is a
		// warning rather than a refusal: the disk in front of it is a volume
		// that survived the restart, so it is warm, and refusing to serve
		// from it would turn one back-end's outage into a cache outage for
		// every job on the estate. It stays in the chain, because an object
		// store that is down at 09:00 is usually up at 09:05.
		rctx, cancel := context.WithTimeout(ctx, bucketProbeTimeout)
		if err := b.Reachable(rctx); err != nil {
			log.Warn("object store is not answering; starting anyway and serving from the disk tier",
				"bucket", cfg.Store.Bucket, "endpoint", cfg.Store.Endpoint, "err", err)
		}
		cancel()

		queue = bucket.NewQueue(b, cfg.Upload)
		queue.Start(ctx)
		tiers = append(tiers, b)

		// Uploads go behind the request. A job that has its object on the
		// server's disk is already served; making it wait for the object
		// store as well would charge every build for the slowest tier.
		opts = append(opts, chain.WriteBehind(queue.WriteBehind()))
	}
	ch := chain.New(tiers, opts...)

	collector := gc.New(d, cfg.GC)
	collector.Start(ctx)
	defer collector.Stop()

	frontends, err := buildFrontends(cfg, ch, log)
	if err != nil {
		return cli.Exit(fmt.Sprintf("ci-cache: refusing to start: %v", err), 1)
	}

	srvOpts := []server.Option{
		server.WithLogger(log),
		server.WithFrontends(frontends...),
		server.WithDiskCheck(func(context.Context) error { return checkWritable(cfg.Persistence.Dir) }),
	}
	if b != nil {
		srvOpts = append(srvOpts, server.WithBucketCheck(b.Reachable))
	}
	srv, err := server.New(cfg, ch, srvOpts...)
	if err != nil {
		return cli.Exit(fmt.Sprintf("ci-cache: refusing to start: %v", err), 1)
	}

	log.Info("starting",
		"version", version,
		"data_port", cfg.Service.Data,
		"dir", cfg.Persistence.Dir,
		"bucket", cfg.Store.Bucket,
		"frontends", frontendNames(frontends),
		"degraded", queue == nil)

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe(ctx) }()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	case s := <-sigs:
		log.Info("draining", "signal", s.String(), "timeout", cfg.Server.DrainTimeout.String())
	}

	// A second interrupt is somebody who has stopped waiting. Honouring it
	// costs whatever has not been flushed, which is the trade they just
	// made; ignoring it costs them the only lever they have left.
	go func() {
		<-sigs
		fmt.Fprintln(os.Stderr, "ci-cache: second interrupt, exiting without finishing the drain")
		os.Exit(130)
	}()

	// The drain runs on a fresh context: ctx may be the very thing that was
	// cancelled, and a shutdown that inherits a dead deadline finishes
	// nothing at all.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.Server.DrainTimeout)
	defer cancel()

	if err := srv.Shutdown(dctx); err != nil {
		log.Warn("listeners did not close cleanly", "err", err)
	}
	if queue != nil {
		drainQueue(dctx, log, queue)
	}
	collector.Stop()

	// Closing the disk tier is what writes its index. Without it the next
	// start walks the whole volume to rediscover what is on it, and serves
	// with an incomplete byte total until it finishes.
	if err := d.Close(); err != nil {
		log.Warn("disk index not persisted; the next start will rebuild it", "err", err)
	}

	log.Info("stopped")
	return nil
}

// flusher is the part of the upload queue a drain needs. It is an interface
// so that the drain can be tested against a queue with a slow sink, which is
// the only interesting case and the one a real bucket will not reproduce on
// demand.
type flusher interface {
	Stop(ctx context.Context) error
	Depth() int
}

// drainQueue flushes what is waiting for the object store, and says what was
// left when the clock ran out.
//
// Exiting non-zero here would be wrong: the objects that did not make it are
// on the disk of a pod that is going away, so they are lost either way, and a
// failing exit code turns an ordinary rollout into a CrashLoopBackOff. What
// is owed is a count, so that a queue which never drains is visible as a
// pattern across restarts rather than as one bad night.
func drainQueue(ctx context.Context, log *slog.Logger, q flusher) {
	start := time.Now()
	err := q.Stop(ctx)
	left := q.Depth()

	switch {
	case err != nil:
		log.Warn("upload queue did not drain", "remaining", left,
			"elapsed", time.Since(start).Round(time.Millisecond).String(), "err", err)
	case left > 0:
		log.Warn("upload queue did not drain within the drain timeout", "remaining", left,
			"elapsed", time.Since(start).Round(time.Millisecond).String())
	default:
		log.Info("upload queue drained", "elapsed", time.Since(start).Round(time.Millisecond).String())
	}
}

// buildFrontends constructs the protocols this installation serves.
//
// Only the ones that are switched on: a front-end that is mounted but unused
// still proxies to an upstream the moment somebody guesses its path, and an
// installation that serves protocols nobody asked for is a larger thing to
// reason about than the one that was deployed.
//
// The Go BUILD cache is not here. It is served over cache.v1.Cache, which the
// server mounts itself, because a runner's agent reaches it as a tier and not
// as an HTTP front-end.
func buildFrontends(cfg config.Config, ch tier.Tier, log *slog.Logger) ([]server.Frontend, error) {
	var out []server.Frontend
	if cfg.Frontends.Go.Mod.Enabled {
		p, err := gomod.New(ch, cfg.Frontends.Go.Mod, gomod.WithLogger(log))
		if err != nil {
			return nil, fmt.Errorf("frontends.go.mod: %w", err)
		}
		out = append(out, p)
	}
	return out, nil
}

func frontendNames(fs []server.Frontend) []string {
	names := make([]string, 0, len(fs))
	for _, f := range fs {
		names = append(names, f.Name())
	}
	return names
}

// checkWritable proves the volume is writable before anything depends on it.
//
// A read-only mount, a wrong fsGroup or a volume that never attached all look
// identical from inside the pod until the first write, which is a request
// somebody is waiting on. Spending one file at start-up moves that discovery
// to the deploy.
func checkWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("persistence.dir %q: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".writable-*")
	if err != nil {
		return fmt.Errorf("persistence.dir %q is not writable: %w", dir, err)
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return fmt.Errorf("persistence.dir %q is not writable: %w", dir, err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("persistence.dir %q: %w", filepath.Dir(name), err)
	}
	return nil
}
