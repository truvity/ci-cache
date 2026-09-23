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
		Label:    a.opts.Label,
		Degraded: a.degraded,
		Elapsed:  time.Since(a.started),
		Gets:     a.gets.Load(),
		Hits:     a.hits.Load(),
		Drops:    a.drops.Load(),
		Tiers:    make([]TierStats, 0, len(a.meters)),
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

	var errs int64
	for _, t := range s.Tiers {
		errs += t.Errors + t.PutErrors
	}
	fmt.Fprintf(&b, " read=%s wrote=%s drops=%d errors=%d degraded=%t elapsed=%s",
		humanBytes(s.BytesRead), humanBytes(s.BytesWritten),
		s.Drops, errs, s.Degraded, s.Elapsed.Round(time.Millisecond))
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
