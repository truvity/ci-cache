package bench

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"time"
)

// Sample is one request's outcome.
//
// FirstByte and Complete are separate because they answer different
// questions and a benchmark that reports only one of them hides the thing we
// are chasing. First byte is the round trip plus whatever the server does
// before it starts writing -- which, while the chain buffers a fault-in, is
// the whole object. Complete adds the transfer. A change that streams properly
// moves first byte a long way and complete hardly at all, and a single
// "latency" number would show that as noise.
type Sample struct {
	FirstByte time.Duration
	Complete  time.Duration
	Bytes     int64
	Err       error
}

// Report is what one point on the curve produced.
//
// It is marshalled to JSON so two runs diff, and printed as a line so a
// sweep is readable in a terminal. Both come from the same struct, because a
// summary that is computed twice is a summary that disagrees with itself.
type Report struct {
	Scenario    string  `json:"scenario"`
	Concurrency int     `json:"concurrency"`
	Duration    float64 `json:"duration_s"`

	Requests int   `json:"requests"`
	Errors   int   `json:"errors"`
	Bytes    int64 `json:"bytes"`

	RequestsPerSecond float64 `json:"requests_per_second"`
	MegabytesPerSec   float64 `json:"megabytes_per_second"`

	FirstByteP50 float64 `json:"first_byte_p50_ms"`
	FirstByteP95 float64 `json:"first_byte_p95_ms"`
	FirstByteP99 float64 `json:"first_byte_p99_ms"`
	CompleteP50  float64 `json:"complete_p50_ms"`
	CompleteP95  float64 `json:"complete_p95_ms"`
	CompleteP99  float64 `json:"complete_p99_ms"`

	// ServerRSSPeak is the server's peak resident memory during the run, in
	// bytes, or 0 when it could not be read. It is the number that decides
	// whether a concurrency increase is safe, and it is the one a
	// client-side load generator cannot see -- so it is filled in by
	// whatever is watching the server, not by the workers.
	ServerRSSPeak int64 `json:"server_rss_peak_bytes,omitempty"`

	// Notes carries anything that qualifies the numbers: a corpus that did
	// not fully populate, a scenario that could not wipe the disk. A report
	// with notes is still a report, but it is not a clean one.
	Notes []string `json:"notes,omitempty"`
}

// Summarise turns samples into a report.
//
// Percentiles come from sorting the whole sample set rather than from a
// streaming estimator. We control the sample count -- a 60-second run at
// realistic rates is tens of thousands of requests, which sorts in
// microseconds -- and an exact p99 is worth more than a cheap one when p99
// is the number a change has to move.
func Summarise(scenario string, concurrency int, elapsed time.Duration, samples []Sample) Report {
	r := Report{
		Scenario:    scenario,
		Concurrency: concurrency,
		Duration:    elapsed.Seconds(),
		Requests:    len(samples),
	}

	fb := make([]float64, 0, len(samples))
	cp := make([]float64, 0, len(samples))

	for _, s := range samples {
		if s.Err != nil {
			r.Errors++

			continue
		}

		r.Bytes += s.Bytes
		fb = append(fb, float64(s.FirstByte)/float64(time.Millisecond))
		cp = append(cp, float64(s.Complete)/float64(time.Millisecond))
	}

	if elapsed > 0 {
		r.RequestsPerSecond = float64(len(samples)) / elapsed.Seconds()
		r.MegabytesPerSec = float64(r.Bytes) / 1e6 / elapsed.Seconds()
	}

	sort.Float64s(fb)
	sort.Float64s(cp)

	r.FirstByteP50, r.FirstByteP95, r.FirstByteP99 = percentiles(fb)
	r.CompleteP50, r.CompleteP95, r.CompleteP99 = percentiles(cp)

	return r
}

// percentiles returns p50, p95 and p99 of a sorted slice.
func percentiles(sorted []float64) (p50, p95, p99 float64) {
	if len(sorted) == 0 {
		return 0, 0, 0
	}

	at := func(q float64) float64 {
		i := int(math.Ceil(q*float64(len(sorted)))) - 1
		if i < 0 {
			i = 0
		}

		if i >= len(sorted) {
			i = len(sorted) - 1
		}

		return sorted[i]
	}

	return at(0.50), at(0.95), at(0.99)
}

// Line is the one-line form, for watching a sweep.
func (r Report) Line() string {
	s := fmt.Sprintf(
		"%-12s c=%-5d %7.1f MB/s %8.0f req/s  first-byte p50/p95/p99 %6.1f/%7.1f/%7.1f ms  complete p99 %7.1f ms  errors %d",
		r.Scenario, r.Concurrency, r.MegabytesPerSec, r.RequestsPerSecond,
		r.FirstByteP50, r.FirstByteP95, r.FirstByteP99, r.CompleteP99, r.Errors,
	)

	if r.ServerRSSPeak > 0 {
		s += fmt.Sprintf("  server-rss %s", humanBytes(r.ServerRSSPeak))
	}

	for _, n := range r.Notes {
		s += "\n             note: " + n
	}

	return s
}

// WriteJSON appends the report as one JSON object per line.
//
// One object per line rather than an array: a sweep that is interrupted
// still leaves a readable file, and appending a point needs no rewrite.
func (r Report) WriteJSON(w io.Writer) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintln(w, string(b))

	return err
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fkB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
