// Command ci-cache-bench loads a running cache server and reports what it did.
//
// It is a separate binary from ci-cache on purpose. The server ships in a
// distroless image sized to the byte, and a load generator has no business
// in it: nothing in production should be able to point the cache at itself
// and fill a bucket. This one is built by `just bench` and by the benchmark
// Job, and by nothing else.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"
	"golang.org/x/net/http2"

	adminv1 "github.com/truvity/ci-cache/gen/admin/v1"
	"github.com/truvity/ci-cache/gen/admin/v1/adminv1connect"

	"github.com/truvity/ci-cache/bench"
	"github.com/truvity/ci-cache/engine/remote"
)

func main() {
	// The body is a separate function so that os.Exit happens in a frame
	// with no defers. Calling it alongside `defer stop()` would leave the
	// signal handler installed on the way out, which gocritic flags and
	// which would matter the day this grows a cleanup that must run.
	if err := execute(); err != nil {
		fmt.Fprintln(os.Stderr, "ci-cache-bench:", err)
		os.Exit(1)
	}
}

func execute() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cmd := &cli.Command{
		Name:  "ci-cache-bench",
		Usage: "load a cache server and report throughput, latency and the server's memory",
		Description: "Every point is one scenario at one concurrency. The sweep runs\n" +
			"several and appends them to one file, so two builds diff.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "server",
				Usage:    "data port base URL, including the front-end path (.../go/build)",
				Required: true,
			},
			&cli.StringFlag{
				Name:  "admin",
				Usage: "admin port base URL; needed by every scenario but warm-disk, which is the only one that leaves the disk alone",
			},
			&cli.StringFlag{
				Name:  "scenario",
				Value: string(bench.WarmDisk),
				Usage: "warm-disk | cold-bucket | mixed",
			},
			&cli.StringFlag{
				Name:  "concurrency",
				Value: "64",
				Usage: "in-flight requests; a comma-separated list sweeps them in order",
			},
			&cli.DurationFlag{
				Name:  "duration",
				Value: 60 * time.Second,
				Usage: "how long to hold each point",
			},
			&cli.IntFlag{
				Name:  "lookups",
				Value: 2000,
				Usage: "corpus size in lookups; each is one action record and one output",
			},
			&cli.IntFlag{
				Name:  "seed",
				Value: 1,
				Usage: "corpus seed; the same seed is the same bytes in the same order",
			},
			&cli.FloatFlag{
				Name:  "hit-ratio",
				Value: 0.9,
				Usage: "mixed only: the share served from disk, defaulting to the ratio measured on gitops",
			},
			&cli.FloatFlag{
				Name:  "compressible",
				Value: 0.5,
				Usage: "0 incompressible, 1 a single repeated byte; sweep it for the compression work rather than trusting one value",
			},
			&cli.BoolFlag{
				Name:  "populate",
				Value: true,
				Usage: "write the corpus before the first point; off when the server already holds it",
			},
			&cli.StringFlag{
				Name:  "report",
				Usage: "append one JSON object per point to this file",
			},
			&cli.StringFlag{
				Name:  "key-prefix",
				Usage: "segregate this run's keys under go/build/<prefix>/; empty is the faithful shape, a value is for a cache real jobs depend on",
			},
			&cli.BoolFlag{
				Name:  "cleanup",
				Usage: "delete the corpus from the bucket when the sweep finishes; requires --key-prefix, because without one the corpus root IS the real cache root",
			},
			&cli.StringFlag{
				Name:  "transport",
				Value: "h2c",
				Usage: "h2c | http1 — the agent multiplexes onto one h2c connection, and whether that is the wall is a question the bench should be able to ask",
			},
		},
		Action: run,
	}

	return cmd.Run(ctx, os.Args)
}

func run(ctx context.Context, cmd *cli.Command) error {
	levels, err := parseConcurrency(cmd.String("concurrency"))
	if err != nil {
		return err
	}

	scenario := bench.Scenario(cmd.String("scenario"))
	switch scenario {
	case bench.WarmDisk, bench.ColdBucket, bench.Mixed:
	default:
		return fmt.Errorf("unknown scenario %q", scenario)
	}

	// The admin port is how every scenario but warm-disk reaches its
	// starting state. Saying so here, before a corpus is written, is worth
	// more than discovering it after populating several gigabytes.
	var (
		admin *adminWiper
		wiper bench.DiskWiper
	)

	if addr := cmd.String("admin"); addr != "" {
		admin = &adminWiper{client: adminv1connect.NewAdminServiceClient(http.DefaultClient, addr)}
		wiper = admin
	} else if scenario != bench.WarmDisk {
		return fmt.Errorf("scenario %q needs --admin to put the server into its starting state", scenario)
	}

	// Refused here rather than after the run, because the failure mode is
	// deleting the cache every job in the cluster is using. --cleanup wipes
	// the corpus root, and without a key prefix the corpus root is
	// "go/build/" -- the whole Go build cache.
	cleanup := cmd.Bool("cleanup")
	if cleanup {
		if cmd.String("key-prefix") == "" {
			return errors.New("--cleanup without --key-prefix would wipe the whole go/build cache, not this run's corpus")
		}

		if admin == nil {
			return errors.New("--cleanup needs --admin")
		}
	}

	corpus := bench.NewCorpus(cmd.Int("lookups"), int64(cmd.Int("seed")), cmd.String("key-prefix"))

	fmt.Fprintf(os.Stderr, "corpus: %d lookups, %d objects, %s under %s; scenario %s; transport %s\n",
		len(corpus.Lookups), len(corpus.Objects), human(corpus.Bytes()), corpus.Root, scenario, cmd.String("transport"))

	// One client across every point in the sweep, so the connection pool is
	// warm for all of them and the first point is not penalised for the TLS
	// and TCP setup the others inherit.
	store, err := newStore(cmd.String("server"), cmd.String("transport"), maxOf(levels))
	if err != nil {
		return err
	}

	var out *os.File
	if path := cmd.String("report"); path != "" {
		out, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}

		// Reported rather than ignored: a failed close can mean the last
		// point of a sweep never reached the disk, and a sweep that silently
		// loses its final row is worse than one that says so.
		defer func() {
			if cerr := out.Close(); cerr != nil {
				fmt.Fprintln(os.Stderr, "ci-cache-bench: closing report:", cerr)
			}
		}()
	}

	populate := cmd.Bool("populate")

	for _, c := range levels {
		rep, err := bench.Run(ctx, bench.Config{
			Scenario:     scenario,
			Concurrency:  c,
			Duration:     cmd.Duration("duration"),
			Corpus:       corpus,
			HitRatio:     cmd.Float("hit-ratio"),
			Compressible: cmd.Float("compressible"),
			Seed:         int64(cmd.Int("seed")),
			Populate:     populate,
		}, store, wiper)
		if err != nil {
			return err
		}

		// Only the first point populates: the corpus is already there for
		// the rest, and rewriting it would measure the write path in the
		// middle of a read benchmark.
		populate = false

		fmt.Println(rep.Line())

		if out != nil {
			if err := rep.WriteJSON(out); err != nil {
				return err
			}
		}
	}

	if cleanup {
		// Not deferred and not on the context under measurement: a run
		// interrupted by ^C should leave the corpus behind to be cleaned up
		// deliberately, rather than racing a cancelled context and deleting
		// half of it while reporting success.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()

		n, err := admin.wipeBucket(ctx, corpus.Root)
		if err != nil {
			return fmt.Errorf("cleanup %s: %w", corpus.Root, err)
		}

		fmt.Fprintf(os.Stderr, "cleanup: removed %d entries under %s\n", n, corpus.Root)
	}

	return nil
}

// newStore builds the client the workers share.
func newStore(server, transport string, maxConcurrency int) (bench.Store, error) {
	var hc *http.Client

	switch transport {
	case "h2c":
		// What the agent does today: one connection, every request
		// multiplexed onto it, which is the arrangement under suspicion.
		hc = &http.Client{Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}}
	case "http1":
		// Many connections, which is what go-cache-plugin had against S3.
		// The pool is sized to the load so the transport is not the limit
		// being measured.
		hc = &http.Client{Transport: &http.Transport{
			MaxIdleConns:        maxConcurrency * 2,
			MaxIdleConnsPerHost: maxConcurrency * 2,
			MaxConnsPerHost:     maxConcurrency * 2,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   false,
		}}
	default:
		return nil, fmt.Errorf("unknown transport %q: h2c or http1", transport)
	}

	return remote.New(server, remote.WithHTTPClient(hc), remote.WithName("bench"))
}

// adminWiper adapts the generated admin client to what the bench needs.
type adminWiper struct {
	client adminv1connect.AdminServiceClient
}

func (a *adminWiper) WipeDisk(ctx context.Context, prefix string) error {
	_, err := a.client.WipeDisk(ctx, connect.NewRequest(&adminv1.WipeDiskRequest{PrefixOrKey: prefix}))

	return err
}

func (a *adminWiper) wipeBucket(ctx context.Context, prefix string) (int64, error) {
	res, err := a.client.WipeBucket(ctx, connect.NewRequest(&adminv1.WipeBucketRequest{Prefix: prefix}))
	if err != nil {
		return 0, err
	}

	return res.Msg.GetRemoved().GetEntries(), nil
}

func parseConcurrency(s string) ([]int, error) {
	var out []int

	for _, f := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("concurrency %q: want positive integers, comma separated", s)
		}

		out = append(out, n)
	}

	if len(out) == 0 {
		return nil, errors.New("no concurrency levels given")
	}

	return out, nil
}

func maxOf(xs []int) int {
	m := 0
	for _, x := range xs {
		if x > m {
			m = x
		}
	}

	return m
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%dkB", n/(1<<10))
	}
}
