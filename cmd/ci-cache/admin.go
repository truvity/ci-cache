package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/urfave/cli/v3"

	adminv1 "github.com/truvity/ci-cache/gen/admin/v1"
	"github.com/truvity/ci-cache/gen/admin/v1/adminv1connect"
)

// defaultAdminAddr is the admin listener on the loopback of whatever is
// running the server. It is never a service address on purpose: reaching the
// admin API is meant to require a port-forward or a deliberate route, because
// a caller who can wipe can empty the cache for the whole estate.
const defaultAdminAddr = "http://localhost:8081"

const envAdminAddr = envPrefix + "ADMIN_ADDR"

// listPageSize is a page a terminal can scroll and a server can answer
// without walking its whole index for one screen.
const listPageSize = 500

func adminCommand() *cli.Command {
	return &cli.Command{
		Name:  "admin",
		Usage: "talk to a running cache server's administrative API",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "addr",
				Value:   defaultAdminAddr,
				Usage:   "admin API base URL",
				Sources: cli.EnvVars(envAdminAddr),
			},
		},
		Commands: []*cli.Command{
			{
				Name:   "stats",
				Usage:  "what the cache holds and what it has done",
				Action: adminStats,
			},
			{
				Name:      "list",
				Usage:     "list what is on the disk tier under a prefix",
				ArgsUsage: "<prefix>",
				Action:    adminList,
			},
			{
				Name:      "wipe-disk",
				Usage:     "remove a prefix or an exact key from the disk tier",
				ArgsUsage: "<prefix|key>",
				Action:    adminWipeDisk,
			},
			{
				Name:      "wipe-bucket",
				Usage:     "remove a prefix from the object store; this is the one that does not come back",
				ArgsUsage: "<prefix>",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "yes", Usage: "actually do it"},
				},
				Action: adminWipeBucket,
			},
			{
				Name:      "invalidate",
				Usage:     "drop cached entries under a prefix so the next read refetches them",
				ArgsUsage: "<prefix>",
				Action:    adminInvalidate,
			},
		},
	}
}

// adminClient dials the admin API. Connect over plain HTTP/1.1 is enough: the
// calls here are unary, and the client runs next to the port it is reaching.
func adminClient(cmd *cli.Command) adminv1connect.AdminServiceClient {
	addr := strings.TrimSuffix(cmd.Root().String("addr"), "/")
	if s := cmd.String("addr"); s != "" {
		addr = strings.TrimSuffix(s, "/")
	}
	return adminv1connect.NewAdminServiceClient(http.DefaultClient, addr)
}

func adminStats(ctx context.Context, cmd *cli.Command) error {
	res, err := adminClient(cmd).Stats(ctx, connect.NewRequest(&adminv1.StatsRequest{}))
	if err != nil {
		return adminFailure(err)
	}
	s := res.Msg
	w := cmd.Root().Writer

	out(w, "version            %s\n", s.GetVersion())
	if t := s.GetStartedAt(); t != nil {
		out(w, "uptime             %s\n", time.Since(t.AsTime()).Round(time.Second))
	}
	out(w, "disk               %s of %s\n",
		humanBytes(s.GetDiskUsedBytes()), humanBytes(s.GetDiskBudgetBytes()))
	out(w, "upload queue       %d\n", s.GetUploadQueueDepth())
	if s.GetIndexCold() {
		out(w, "index              still warming: the counters below are incomplete"+"\n")
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	out(tw, "\nFRONTEND\tTIER\tGETS\tHITS\tMISSES\tERRORS\tPUTS\tREAD\tWRITTEN"+"\n")
	for _, f := range s.GetFrontends() {
		out(tw, "%s\t\t\t\t\t\t\t%s\t\n", f.GetFrontend(), humanBytes(f.GetBytes()))
		for _, t := range f.GetTiers() {
			out(tw, "\t%s\t%d\t%d\t%d\t%d\t%d\t%s\t%s\n",
				t.GetTier(), t.GetGets(), t.GetHits(), t.GetMisses(), t.GetErrors(), t.GetPuts(),
				humanBytes(t.GetBytesRead()), humanBytes(t.GetBytesWritten()))
		}
	}
	return tw.Flush()
}

func adminList(ctx context.Context, cmd *cli.Command) error {
	prefix, err := oneArg(cmd, "prefix")
	if err != nil {
		return err
	}
	client := adminClient(cmd)
	w := cmd.Root().Writer

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	var total int64
	for token := ""; ; {
		res, err := client.List(ctx, connect.NewRequest(&adminv1.ListRequest{
			Prefix: prefix, PageSize: listPageSize, PageToken: token,
		}))
		if err != nil {
			return adminFailure(err)
		}
		for _, e := range res.Msg.GetEntries() {
			last := ""
			if t := e.GetLastAccess(); t != nil {
				last = t.AsTime().Format(time.RFC3339)
			}
			out(tw, "%s\t%s\t%s\n", e.GetKey(), humanBytes(e.GetSize()), last)
			total += e.GetSize()
		}
		token = res.Msg.GetNextPageToken()
		if token == "" {
			break
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	out(w, "\n%s under %q\n", humanBytes(total), prefix)
	return nil
}

func adminWipeDisk(ctx context.Context, cmd *cli.Command) error {
	target, err := oneArg(cmd, "prefix|key")
	if err != nil {
		return err
	}
	res, err := adminClient(cmd).WipeDisk(ctx, connect.NewRequest(&adminv1.WipeDiskRequest{
		PrefixOrKey: target,
	}))
	if err != nil {
		return adminFailure(err)
	}
	printRemoved(cmd.Root().Writer, "removed from disk", res.Msg.GetRemoved())
	return nil
}

// adminWipeBucket is the one call here that destroys something the cache
// cannot rebuild, so without --yes it reports and stops.
//
// Exit 2 and not 0: this is a refusal, and a script that treats "I did
// nothing" as success will happily run the whole wipe playbook believing each
// step worked. Exit 1 would be indistinguishable from the server being
// unreachable, which is a different thing to do next.
func adminWipeBucket(ctx context.Context, cmd *cli.Command) error {
	prefix, err := oneArg(cmd, "prefix")
	if err != nil {
		return err
	}
	if !cmd.Bool("yes") {
		return wipeBucketDryRun(ctx, cmd, prefix)
	}
	res, err := adminClient(cmd).WipeBucket(ctx, connect.NewRequest(&adminv1.WipeBucketRequest{
		Prefix: prefix,
	}))
	if err != nil {
		return adminFailure(err)
	}
	w := cmd.Root().Writer
	printRemoved(w, "removed from the object store", res.Msg.GetRemoved())
	printRemoved(w, "removed from disk with it", res.Msg.GetRemovedFromDisk())
	return nil
}

func wipeBucketDryRun(ctx context.Context, cmd *cli.Command, prefix string) error {
	w := cmd.Root().Writer
	out(w, "would remove everything under %q from the object store.\n", prefix)

	// The disk listing is shown because it is the only inventory the admin
	// API has, and it is labelled as a lower bound because it is one: the
	// bucket holds everything the disk has evicted as well.
	client := adminClient(cmd)
	var entries, bytes int64
	for token := ""; ; {
		res, err := client.List(ctx, connect.NewRequest(&adminv1.ListRequest{
			Prefix: prefix, PageSize: listPageSize, PageToken: token,
		}))
		if err != nil {
			out(w, "could not size it from the disk index: %v\n", err)
			break
		}
		for _, e := range res.Msg.GetEntries() {
			entries++
			bytes += e.GetSize()
		}
		if token = res.Msg.GetNextPageToken(); token == "" {
			break
		}
	}
	out(w, "the disk tier holds %d entries (%s) under it; the object store holds at least that.\n",
		entries, humanBytes(bytes))
	return cli.Exit("re-run with --yes to do it", 2)
}

func adminInvalidate(ctx context.Context, cmd *cli.Command) error {
	prefix, err := oneArg(cmd, "prefix")
	if err != nil {
		return err
	}
	res, err := adminClient(cmd).Invalidate(ctx, connect.NewRequest(&adminv1.InvalidateRequest{
		Prefix: prefix,
	}))
	if err != nil {
		return adminFailure(err)
	}
	printRemoved(cmd.Root().Writer, "invalidated", res.Msg.GetRemoved())
	return nil
}

// out writes one line of human-readable output.
//
// The error is dropped deliberately. This is a terminal or a pipe somebody
// closed, and a CLI that reports its own broken pipe has turned
// `ci-cache admin list | head` into a failure.
func out(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func printRemoved(w io.Writer, what string, r *adminv1.Removed) {
	// A wipe that matched nothing is not a failure, and printing the zero is
	// how the caller tells "nothing was there" from "it did not run".
	out(w, "%s: %d entries, %s\n", what, r.GetEntries(), humanBytes(r.GetBytes()))
}

func oneArg(cmd *cli.Command, name string) (string, error) {
	if cmd.Args().Len() != 1 {
		return "", cli.Exit(fmt.Sprintf("ci-cache: %s takes exactly one argument: <%s>", cmd.Name, name), 1)
	}
	return cmd.Args().First(), nil
}

// adminFailure turns a Connect error into the sentence a person reading a
// terminal needs, which is almost always about the address rather than the
// call: the admin port is not routed by default, so "connection refused" here
// usually means the port-forward, not the server.
func adminFailure(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return cli.Exit(fmt.Sprintf("ci-cache: admin API: %s: %s", ce.Code(), ce.Message()), 1)
	}
	return cli.Exit(fmt.Sprintf("ci-cache: admin API: %v", err), 1)
}

// humanBytes is the CLI's own copy of the short byte format. The agent has
// one too, and they are deliberately not shared: this one is read in a
// terminal next to a column of others, and tying the two together would mean
// a change for one showing up in the other's output.
func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "kMGTPE"[exp])
}
