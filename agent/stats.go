package agent

import (
	"fmt"
	"strings"
	"time"
)

// TierStats is one tier's counters for the life of the agent.
type TierStats struct {
	Tier         string
	Gets         int64
	Hits         int64
	Misses       int64
	Errors       int64
	Puts         int64
	PutErrors    int64
	BytesRead    int64
	BytesWritten int64
}

// Stats is what the agent did, in the form a summary line needs.
type Stats struct {
	Label    string
	Degraded bool
	Elapsed  time.Duration

	// Gets and Hits count what the TOOLCHAIN asked for: one per compile
	// action. Tiers counts tier operations, and there are more of those --
	// answering one action reads its record and then its output.
	Gets   int64
	Hits   int64
	Misses int64

	// Drops are objects the chain would not take. Each one is somebody's
	// slower build later and nothing worse now, which is why it is a count
	// and not an error.
	Drops int64

	// Reused are gets answered from a file this build had already
	// materialised: no body fetched, nothing written. It is reported because
	// a conditional fetch that silently never fires is indistinguishable
	// from one that is not there.
	Reused int64

	// LostRecords are objects that reached local disk but never reached the
	// chain: a drain that ran out of time, or a file that could not be
	// reopened. Distinct from Drops, which the chain saw and refused.
	//
	// It is on the summary line because recording moved off the critical
	// path, and the failure mode that introduces is a build that looks
	// faster because it recorded less. A number that is not zero is the
	// first thing to check when a time improves.
	LostRecords int64

	BytesRead    int64
	BytesWritten int64

	Tiers []TierStats
}

// Stats reads the counters.
//
// They are read one at a time and so do not describe a single instant.
// Nothing here is a ledger, and the summary is printed as the process exits,
// with nothing left racing it.
func (a *Agent) Stats() Stats {
	s := Stats{
		Label:       a.opts.Label,
		Degraded:    a.degraded,
		Elapsed:     time.Since(a.started),
		Gets:        a.gets.Load(),
		Hits:        a.hits.Load(),
		Drops:       a.drops.Load(),
		LostRecords: a.lostRecords.Load(),
		Reused:      a.reused.Load(),
		Tiers:       make([]TierStats, 0, len(a.meters)),
	}
	s.Misses = s.Gets - s.Hits
	for _, m := range a.meters {
		t := m.snapshot()
		s.BytesRead += t.BytesRead
		s.BytesWritten += t.BytesWritten
		s.Tiers = append(s.Tiers, t)
	}
	return s
}

// Line is the one-line summary printed at exit.
//
// One line, because it is read in a job log next to a thousand others, and it
// is read by somebody asking one question: did the cache work? The per-tier
// breakdown answers it -- hits at the front mean a warm runner, hits at the
// back mean the network paid for every one of them, and no hits at all mean
// the cache is not doing anything and somebody should find out why.
func (s Stats) Line() string {
	var b strings.Builder
	b.WriteString("ci-cache agent")
	if s.Label != "" {
		fmt.Fprintf(&b, "[%s]", s.Label)
	}
	fmt.Fprintf(&b, ": gets=%d hits=%d misses=%d", s.Gets, s.Hits, s.Misses)

	parts := make([]string, 0, len(s.Tiers))
	for _, t := range s.Tiers {
		parts = append(parts, fmt.Sprintf("%s:%d", t.Tier, t.Hits))
	}

	if len(parts) > 0 {
		fmt.Fprintf(&b, " tier-hits=%s", strings.Join(parts, ","))
	}

	// Per-tier bytes, because the totals cannot answer the question that
	// actually matters: which tier is being written to, and is anything
	// reading it back. Two rounds of this work were spent guessing at that
	// from wall-clock alone.
	io := make([]string, 0, len(s.Tiers))
	for _, t := range s.Tiers {
		io = append(io, fmt.Sprintf("%s:r%s/w%s", t.Tier, humanBytes(t.BytesRead), humanBytes(t.BytesWritten)))
	}

	if len(io) > 0 {
		fmt.Fprintf(&b, " tier-io=%s", strings.Join(io, ","))
	}

	var errs int64
	for _, t := range s.Tiers {
		errs += t.Errors + t.PutErrors
	}
	fmt.Fprintf(&b, " reused=%d read=%s wrote=%s drops=%d lost=%d errors=%d degraded=%t elapsed=%s",
		s.Reused, humanBytes(s.BytesRead), humanBytes(s.BytesWritten),
		s.Drops, s.LostRecords, errs, s.Degraded, s.Elapsed.Round(time.Millisecond))
	return b.String()
}

// humanBytes formats a byte count the way a log reader scans one. Exact
// numbers are not the point here: nobody acts differently on 1,203,441 than
// on 1.2MB, and the short form is what makes the line readable at a glance.
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
