package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truvity/ci-cache/agent"
)

// blackhole is a TEST-NET-1 address (RFC 5737). Nothing routes there, so a
// probe against it either refuses immediately or hangs until its timeout --
// both of which are the case this is here to measure.
const blackhole = "http://192.0.2.1:9/go/build"

type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recorder) warnings() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, l := range r.lines {
		if strings.HasPrefix(l, agent.WarnPrefix) {
			out = append(out, l)
		}
	}
	return out
}

// TestStartsDegradedWhenNothingIsReachable is the failure this package exists
// for. A runner whose cache server is down must still build, must not spend
// the build discovering that, and must say so once.
func TestStartsDegradedWhenNothingIsReachable(t *testing.T) {
	rec := &recorder{}
	dir := t.TempDir()

	start := time.Now()
	a, err := agent.New(context.Background(), agent.Options{
		Remote:   blackhole,
		CacheDir: dir,
		Logf:     rec.logf,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("New with an unreachable remote = %v, want an agent: only a local failure may be fatal", err)
	}
	defer a.Close()

	if elapsed > 2500*time.Millisecond {
		t.Errorf("start-up took %s, want at most 2.5s: the toolchain is blocked for all of it", elapsed)
	}
	if !a.Degraded() {
		t.Error("Degraded = false, want true: the chain that came up is not the one that was asked for")
	}
	if got, want := a.TierNames(), []string{"disk"}; len(got) != 1 || got[0] != want[0] {
		t.Errorf("TierNames = %v, want %v", got, want)
	}
	if warns := rec.warnings(); len(warns) != 1 {
		t.Errorf("logged %d warnings, want exactly 1: %v", len(warns), warns)
	} else if !strings.Contains(warns[0], "192.0.2.1:9") {
		t.Errorf("warning does not name the address: %q", warns[0])
	}
}

func TestLocalOnlyChainIsNotDegraded(t *testing.T) {
	rec := &recorder{}
	a, err := agent.New(context.Background(), agent.Options{CacheDir: t.TempDir(), Logf: rec.logf})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	defer a.Close()

	if a.Degraded() {
		t.Error("Degraded = true with nothing configured behind the disk; there is nothing to be degraded from")
	}
	if warns := rec.warnings(); len(warns) != 0 {
		t.Errorf("logged %d warnings, want none: %v", len(warns), warns)
	}
}

// --- the cache protocol ------------------------------------------------

type progRequest struct {
	ID       int64
	Command  string
	ActionID []byte `json:",omitempty"`
	OutputID []byte `json:",omitempty"`
	BodySize int64  `json:",omitempty"`
}

type progResponse struct {
	ID            int64
	Err           string    `json:",omitempty"`
	KnownCommands []string  `json:",omitempty"`
	Miss          bool      `json:",omitempty"`
	OutputID      []byte    `json:",omitempty"`
	Size          int64     `json:",omitempty"`
	Time          time.Time `json:",omitempty"`
	DiskPath      string    `json:",omitempty"`
}

// session drives the agent the way cmd/go does: one request at a time, each
// answered before the next is written. The server handles requests
// concurrently, so feeding it the whole script at once would make the order
// of the responses a race rather than a test.
type session struct {
	t    *testing.T
	in   *io.PipeWriter
	dec  *json.Decoder
	done chan error
}

func newSession(t *testing.T, a *agent.Agent) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	s := &session{t: t, in: inW, dec: json.NewDecoder(outR), done: make(chan error, 1)}
	go func() {
		err := a.Serve(context.Background(), inR, outW)
		outW.Close()
		s.done <- err
	}()

	// The server greets with the commands it knows before anything is asked
	// of it; a client that does not read it is a client that deadlocks.
	greeting := s.read()
	if greeting.ID != 0 || len(greeting.KnownCommands) == 0 {
		t.Fatalf("greeting = %+v, want ID 0 and a command list", greeting)
	}
	for _, want := range []string{"get", "put", "close"} {
		if !contains(greeting.KnownCommands, want) {
			t.Fatalf("greeting does not advertise %q: %v", want, greeting.KnownCommands)
		}
	}
	return s
}

func (s *session) read() progResponse {
	s.t.Helper()
	var r progResponse
	if err := s.dec.Decode(&r); err != nil {
		s.t.Fatalf("decode response: %v", err)
	}
	return r
}

func (s *session) send(req progRequest, body []byte) progResponse {
	s.t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		s.t.Fatalf("encode request: %v", err)
	}
	if _, err := s.in.Write(append(b, '\n')); err != nil {
		s.t.Fatalf("write request: %v", err)
	}
	if body != nil {
		// A put's body follows its request as a base64 JSON string, which is
		// exactly what encoding/json makes of a []byte.
		bb, err := json.Marshal(body)
		if err != nil {
			s.t.Fatalf("encode body: %v", err)
		}
		if _, err := s.in.Write(append(bb, '\n')); err != nil {
			s.t.Fatalf("write body: %v", err)
		}
	}
	return s.read()
}

func (s *session) close() {
	s.t.Helper()
	s.in.Close()
	select {
	case err := <-s.done:
		if err != nil {
			s.t.Fatalf("Serve = %v, want nil on end of input", err)
		}
	case <-time.After(5 * time.Second):
		s.t.Fatal("Serve did not return after stdin closed")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestProtocolMissThenPutThenHit(t *testing.T) {
	a, err := agent.New(context.Background(), agent.Options{CacheDir: t.TempDir(), Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	defer a.Close()

	s := newSession(t, a)
	action := []byte("\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10")
	output := []byte("\xa0\xa1\xa2\xa3\xa4\xa5\xa6\xa7\xa8\xa9\xaa\xab\xac\xad\xae\xaf")
	body := []byte("xyzzy")

	if got := s.send(progRequest{ID: 1, Command: "get", ActionID: action}, nil); !got.Miss || got.Err != "" {
		t.Fatalf("get on an empty cache = %+v, want a miss", got)
	}

	put := s.send(progRequest{
		ID: 2, Command: "put", ActionID: action, OutputID: output, BodySize: int64(len(body)),
	}, body)
	if put.Err != "" {
		t.Fatalf("put = %q, want it to succeed", put.Err)
	}
	if put.DiskPath == "" {
		t.Fatal("put returned no disk path: the toolchain opens the file itself")
	}
	if got, err := os.ReadFile(put.DiskPath); err != nil || string(got) != string(body) {
		t.Fatalf("object at %s = %q, %v; want %q", put.DiskPath, got, err, body)
	}

	hit := s.send(progRequest{ID: 3, Command: "get", ActionID: action}, nil)
	if hit.Miss || hit.Err != "" {
		t.Fatalf("get after put = %+v, want a hit", hit)
	}
	if string(hit.OutputID) != string(output) {
		t.Errorf("OutputID = %x, want %x", hit.OutputID, output)
	}
	if hit.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", hit.Size, len(body))
	}
	if got, err := os.ReadFile(hit.DiskPath); err != nil || string(got) != string(body) {
		t.Fatalf("object at %s = %q, %v; want %q", hit.DiskPath, got, err, body)
	}

	if got := s.send(progRequest{ID: 4, Command: "close"}, nil); got.Err != "" {
		t.Errorf("close = %q, want it to succeed", got.Err)
	}
	s.close()

	stats := a.Stats()
	if stats.Gets != 2 {
		t.Errorf("Gets = %d, want 2", stats.Gets)
	}
	if stats.Hits != 1 {
		t.Errorf("Hits = %d, want 1", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Errorf("Misses = %d, want 1", stats.Misses)
	}
	if line := stats.Line(); !strings.Contains(line, "degraded=false") || !strings.Contains(line, "gets=2") {
		t.Errorf("summary line = %q, want it to report the run", line)
	}
}

// TestProtocolSurvivesADeadRemote is the whole contract in one test: with the
// server unreachable, every request still gets an answer and none of them is
// an error.
func TestProtocolSurvivesADeadRemote(t *testing.T) {
	rec := &recorder{}
	a, err := agent.New(context.Background(), agent.Options{
		Remote:   blackhole,
		CacheDir: t.TempDir(),
		Logf:     rec.logf,
	})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	defer a.Close()

	s := newSession(t, a)
	action := []byte("\x11\x22\x33\x44")
	output := []byte("\x55\x66\x77\x88")
	body := []byte("still builds")

	if got := s.send(progRequest{ID: 1, Command: "get", ActionID: action}, nil); got.Err != "" {
		t.Fatalf("get with a dead remote = %q, want a miss and no error", got.Err)
	}
	put := s.send(progRequest{
		ID: 2, Command: "put", ActionID: action, OutputID: output, BodySize: int64(len(body)),
	}, body)
	if put.Err != "" {
		t.Fatalf("put with a dead remote = %q, want it to succeed locally", put.Err)
	}
	if got := s.send(progRequest{ID: 3, Command: "get", ActionID: action}, nil); got.Miss || got.Err != "" {
		t.Fatalf("get after put with a dead remote = %+v, want a local hit", got)
	}
	s.close()
}
