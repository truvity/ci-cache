package agent_test

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/truvity/ci-cache/agent"
	"github.com/truvity/ci-cache/config"
	"github.com/truvity/ci-cache/engine/tier/tiertest"
	"github.com/truvity/ci-cache/server"
)

// startServer runs a real cache server over the given tier and returns the
// agent-facing URL.
//
// A real server rather than a stub: the thing under test is where the
// toolchain's wait ends, and a stub that answers instantly cannot show that
// the wait moved. The tier's hooks are what make the server slow on demand.
func startServer(t *testing.T, back *tiertest.Memory) string {
	t.Helper()

	cfg := config.Default()
	cfg.Frontends.Go.Build.Enabled = true

	srv, err := server.New(cfg, back)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- srv.Serve(ctx, ln) }()

	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("server did not stop")
		}
	})

	return "http://" + ln.Addr().String() + "/go/build"
}

// The point of the whole change, stated as a test.
//
// Before: put wrote the object locally and then waited for the chain -- a
// local tier write and an HTTP round trip -- before answering. A `go build`
// writes thousands of objects, so the build paid that round trip thousands
// of times in series. That is the measured gap against go-cache-plugin,
// which answers as soon as the object is on disk.
//
// The assertion is deliberately not a timing threshold. It holds the server
// still and checks that the toolchain was answered anyway, which is the same
// claim without a number that goes flaky on a loaded machine.
func TestAPutIsAnsweredBeforeItIsRecorded(t *testing.T) {
	back := tiertest.NewMemory()

	admitted := make(chan struct{})
	release := make(chan struct{})

	var closedAdmitted bool

	back.BeforePut = func(ctx context.Context, _ string) error {
		if !closedAdmitted {
			closedAdmitted = true
			close(admitted)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	url := startServer(t, back)

	a, err := agent.New(context.Background(), agent.Options{
		Remote:   url,
		CacheDir: t.TempDir(),
		Logf:     func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	defer a.Close()

	if a.Degraded() {
		t.Fatal("agent came up degraded against a live server; the test would prove nothing")
	}

	s := newSession(t, a)
	action := []byte("\x21\x22\x23\x24")
	output := []byte("\x31\x32\x33\x34")
	body := []byte("recorded behind the build")

	put := s.send(progRequest{
		ID: 1, Command: "put", ActionID: action, OutputID: output, BodySize: int64(len(body)),
	}, body)
	if put.Err != "" {
		t.Fatalf("put = %q, want it to succeed", put.Err)
	}

	if put.DiskPath == "" {
		t.Fatal("put returned no disk path")
	}

	// The server is still inside BeforePut and has stored nothing. The
	// toolchain has its answer regardless, which is the whole claim.
	select {
	case <-admitted:
	case <-time.After(10 * time.Second):
		t.Fatal("the recording never reached the server: it is not happening at all")
	}

	if n := back.Len(); n != 0 {
		t.Fatalf("the backing tier already holds %d objects; the put waited for the chain after all", n)
	}

	// Now let the server finish, and close the stream the way cmd/go does.
	// The drain in protocolClose is where the deferred work is paid back.
	close(release)

	if got := s.send(progRequest{ID: 2, Command: "close"}, nil); got.Err != "" {
		t.Fatalf("close = %q", got.Err)
	}

	if n := back.Len(); n == 0 {
		t.Fatal("nothing reached the backing tier after close: the drain did not wait for the recordings")
	}

	s.close()
}

// Deferring the work must not lose it. If close returned before the
// recordings landed, every build would be fast and every following build
// would miss, which is the failure mode that looks like success.
func TestCloseDrainsTheRecordings(t *testing.T) {
	back := tiertest.NewMemory()

	// Slow, but finite: enough that a close which did not wait would
	// reliably observe an empty tier.
	back.BeforePut = func(context.Context, string) error {
		time.Sleep(100 * time.Millisecond)

		return nil
	}

	url := startServer(t, back)

	a, err := agent.New(context.Background(), agent.Options{
		Remote:   url,
		CacheDir: t.TempDir(),
		Logf:     func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	defer a.Close()

	s := newSession(t, a)

	const objects = 8
	for i := range objects {
		action := []byte{0x40, byte(i), 0x00, 0x01}
		output := []byte{0x50, byte(i), 0x00, 0x01}
		body := []byte{byte(i), byte(i), byte(i)}

		put := s.send(progRequest{
			ID: int64(i + 1), Command: "put", ActionID: action, OutputID: output, BodySize: int64(len(body)),
		}, body)
		if put.Err != "" {
			t.Fatalf("put %d = %q", i, put.Err)
		}
	}

	if got := s.send(progRequest{ID: 100, Command: "close"}, nil); got.Err != "" {
		t.Fatalf("close = %q", got.Err)
	}

	// Each object is an action record and an output, so the tier holds two
	// entries per put.
	if got, want := back.Len(), 2*objects; got != want {
		t.Errorf("backing tier holds %d entries after close, want %d: the drain returned early", got, want)
	}

	if st := a.Stats(); st.LostRecords != 0 {
		t.Errorf("%d records lost with a server that answered every call", st.LostRecords)
	}

	s.close()
}

// A server that refuses must slow nobody down and fail nothing. The objects
// are on local disk; only the recording is lost.
func TestARefusingServerDoesNotFailTheBuild(t *testing.T) {
	back := tiertest.NewMemory()
	back.BeforePut = func(context.Context, string) error {
		return errContrived
	}

	url := startServer(t, back)

	a, err := agent.New(context.Background(), agent.Options{
		Remote:   url,
		CacheDir: t.TempDir(),
		Logf:     func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	defer a.Close()

	s := newSession(t, a)
	action := []byte("\x61\x62\x63\x64")
	output := []byte("\x71\x72\x73\x74")
	body := []byte("still on disk")

	put := s.send(progRequest{
		ID: 1, Command: "put", ActionID: action, OutputID: output, BodySize: int64(len(body)),
	}, body)
	if put.Err != "" {
		t.Fatalf("put = %q, want the build to carry on when the server refuses", put.Err)
	}

	if got := s.send(progRequest{ID: 2, Command: "close"}, nil); got.Err != "" {
		t.Fatalf("close = %q, want it to succeed despite the refusals", got.Err)
	}

	// The object the compiler was handed must still be readable: the local
	// write is the part that is not allowed to be best-effort.
	hit := s.send(progRequest{ID: 3, Command: "get", ActionID: action}, nil)
	if hit.Miss || hit.Err != "" {
		t.Errorf("get after a refused recording = %+v, want a local hit", hit)
	}

	s.close()
}

// errContrived is the refusal injected above. It is a named value so the
// test reads as "the server refused" rather than as a string literal that
// could be mistaken for a real error from the stack.
var errContrived = errors.New("contrived refusal")

// The regression that the split exists to prevent.
//
// A first attempt at this deferred the WHOLE chain write, local tier
// included. Every get for something the build had just produced then missed,
// and the compiler redid the work -- a slower build that looks exactly like
// "the cache is cold" and not at all like a bug. The local tier is therefore
// written before the put is answered, and only what sits behind it is
// deferred.
//
// The server is held still for the duration, so a hit here can only have
// come from the local tier.
func TestAGetAfterAPutHitsWhileTheServerIsStillBusy(t *testing.T) {
	back := tiertest.NewMemory()

	release := make(chan struct{})

	back.BeforePut = func(ctx context.Context, _ string) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	url := startServer(t, back)

	a, err := agent.New(context.Background(), agent.Options{
		Remote:   url,
		CacheDir: t.TempDir(),
		Logf:     func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New = %v", err)
	}

	// Close runs through Cleanup, not defer, and the ordering is the whole
	// point: Close drains the recordings, and a deferred Close would run
	// BEFORE the deferred close(release) that unblocks them -- so the drain
	// would sit out the upload timeout and the test would take a minute to
	// pass. Cleanup runs after the defers, so the server is released first.
	defer close(release)
	t.Cleanup(a.Close)

	if a.Degraded() {
		t.Fatal("agent came up degraded; the test would prove nothing")
	}

	s := newSession(t, a)
	action := []byte("\x81\x82\x83\x84")
	output := []byte("\x91\x92\x93\x94")
	body := []byte("readable immediately")

	if put := s.send(progRequest{
		ID: 1, Command: "put", ActionID: action, OutputID: output, BodySize: int64(len(body)),
	}, body); put.Err != "" {
		t.Fatalf("put = %q", put.Err)
	}

	hit := s.send(progRequest{ID: 2, Command: "get", ActionID: action}, nil)
	if hit.Miss || hit.Err != "" {
		t.Fatalf("get straight after put = %+v, want a hit from the local tier", hit)
	}

	if got, err := os.ReadFile(hit.DiskPath); err != nil || string(got) != string(body) {
		t.Fatalf("object at %s = %q, %v; want %q", hit.DiskPath, got, err, body)
	}

	if n := back.Len(); n != 0 {
		t.Errorf("the server stored %d entries while held still: the deferred write is not deferred", n)
	}
}

// The conditional fetch, stated as a test.
//
// Within one build the toolchain asks repeatedly for objects it has already
// been handed. The action record -- 84 bytes -- is all that is needed to
// discover that, but Get offered no way to act on it. So the agent read the
// whole object back out of a tier and handed it to objectDir.write, which,
// finding the file already present, drained the reader into io.Discard. A
// full read of a megabyte, to learn nothing.
//
// Usually that read is local disk. It is the network when the disk tier has
// been evicted under budget pressure but the materialised file survives --
// which is precisely the constrained runner where it hurts most.
//
// What is asserted here is Reused, because it is the only thing that
// distinguishes the two paths from outside. An assertion that the SERVER was
// not read would pass either way: the put wrote the record and the output
// into the local tier, so a repeat get resolves locally with or without this
// change.
func TestARepeatGetIsServedWithoutFetchingTheBody(t *testing.T) {
	back := tiertest.NewMemory()
	url := startServer(t, back)

	a, err := agent.New(context.Background(), agent.Options{
		Remote:   url,
		CacheDir: t.TempDir(),
		Logf:     func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	t.Cleanup(a.Close)

	if a.Degraded() {
		t.Fatal("agent came up degraded; the test would prove nothing")
	}

	s := newSession(t, a)
	action := []byte("\xa1\xa2\xa3\xa4")
	output := []byte("\xb1\xb2\xb3\xb4")
	body := []byte("fetched exactly once")

	if put := s.send(progRequest{
		ID: 1, Command: "put", ActionID: action, OutputID: output, BodySize: int64(len(body)),
	}, body); put.Err != "" {
		t.Fatalf("put = %q", put.Err)
	}

	hit := s.send(progRequest{ID: 2, Command: "get", ActionID: action}, nil)
	if hit.Miss || hit.Err != "" {
		t.Fatalf("repeat get = %+v, want a hit served from the materialised file", hit)
	}

	got, err := os.ReadFile(hit.DiskPath)
	if err != nil || string(got) != string(body) {
		t.Fatalf("object at %s = %q, %v; want %q", hit.DiskPath, got, err, body)
	}

	if st := a.Stats(); st.Reused == 0 {
		t.Error("Stats().Reused is 0: the body was fetched after all, or the counter is not wired")
	}
}
