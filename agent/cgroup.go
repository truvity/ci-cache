package agent

import (
	"os"
	"strconv"
	"strings"
)

// The agent's local cache lives on the container's filesystem, and on Linux
// every byte it writes there is page cache charged to the container's MEMORY
// limit. So the question "how big may this cache be" is answered by the
// cgroup, not by statfs -- statfs reports the node's disk, which in a
// container is not a bound on anything.
//
// Getting this wrong is not a slow cache, it is a dead job. Uncapped on a
// 14 GiB runner, a Go build went from 76 % of its memory limit with zero
// reclaim events to 100 % with 59,190 of them, and the runner process was
// starved until the control plane lost contact with it (2026-09-23,
// truvity/gitops). The server does not have this problem because it owns a
// volume sized for it; the agent borrows somebody else's.
const (
	// cgroupBudgetPercent is the share of the container's memory limit the
	// local cache may occupy.
	//
	// A quarter, because the tenant that matters is the compiler the cache
	// exists to serve: it is already the largest thing in the cgroup, and a
	// cache that crowds it out has inverted its own purpose. Page cache is
	// reclaimable, so being under is cheap -- a miss goes to the server one
	// hop away -- while being over is paid in reclaim churn by every
	// process in the container at once.
	cgroupBudgetPercent = 25

	// cgroupBudgetFloor keeps a very small container from deriving a budget
	// so tight that the cache thrashes against its own collector.
	cgroupBudgetFloor = 256 << 20

	// noLimit is how an unlimited v1 cgroup reports itself: not a sentinel
	// but the largest page-aligned value the kernel can hold, which reads as
	// an absurd number of bytes rather than as "unlimited".
	noLimit = int64(1) << 53
)

// cgroupLimitPaths are read in order: a v2 host mounts the unified
// hierarchy, a v1 host does not. It is a variable so that a test can point
// it at a directory it controls -- the real files cannot be created, and a
// derivation nobody can test is a derivation nobody can trust.
var cgroupLimitPaths = []string{
	"/sys/fs/cgroup/memory.max",
	"/sys/fs/cgroup/memory/memory.limit_in_bytes",
}

// memoryLimit reports the container's memory limit in bytes.
//
// ok is false when there is no limit to speak of -- not in a container, a
// cgroup that says "max", or a number so large it is the kernel's way of
// saying the same thing. Callers fall back to whatever they did before.
func memoryLimit() (limit int64, ok bool) {
	for _, path := range cgroupLimitPaths {
		n, ok := readLimit(path)
		if ok {
			return n, true
		}
	}
	return 0, false
}

// readLimit parses one cgroup limit file.
func readLimit(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}

	s := strings.TrimSpace(string(b))
	if s == "max" {
		return 0, false
	}

	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n >= noLimit {
		return 0, false
	}

	return n, true
}

// budgetFromMemoryLimit turns a container memory limit into a cache budget,
// or reports false when there is no limit to derive one from.
func budgetFromMemoryLimit() (int64, bool) {
	limit, ok := memoryLimit()
	if !ok {
		return 0, false
	}

	budget := limit / 100 * cgroupBudgetPercent
	if budget < cgroupBudgetFloor {
		budget = cgroupBudgetFloor
	}

	return budget, true
}
