package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// writeLimit puts one cgroup limit file in a temp directory and points the
// package at it.
func writeLimit(t *testing.T, contents ...string) {
	t.Helper()

	dir := t.TempDir()
	paths := make([]string, 0, len(contents))

	for i, c := range contents {
		p := filepath.Join(dir, "limit"+string(rune('0'+i)))
		if c != "" {
			if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		paths = append(paths, p)
	}

	old := cgroupLimitPaths
	cgroupLimitPaths = paths
	t.Cleanup(func() { cgroupLimitPaths = old })
}

// The whole point of the file: in a container the budget comes from the
// memory limit, because the cache's bytes are page cache charged to it.
// Getting this wrong killed a runner rather than slowing it down.
func TestBudgetComesFromTheMemoryLimit(t *testing.T) {
	// The medium ARC runner tier, in the bytes a cgroup actually reports.
	const limit = 14336 << 20

	writeLimit(t, strconv.FormatInt(limit, 10)+"\n")

	got, ok := budgetFromMemoryLimit()
	if !ok {
		t.Fatal("no budget derived from a cgroup that states a limit")
	}

	if want := int64(limit) / 100 * cgroupBudgetPercent; got != want {
		t.Errorf("budget %d, want %d (%d%% of the limit)", got, want, cgroupBudgetPercent)
	}

	if got >= limit {
		t.Errorf("budget %d is not smaller than the limit %d: the compiler has to fit too", got, limit)
	}
}

// Every way a cgroup says "no limit" must read as no limit, so the caller
// falls back to statfs rather than deriving a budget from a sentinel. A v1
// cgroup reports unlimited as a very large number, not as a word.
func TestNoLimitIsNotABudget(t *testing.T) {
	for name, contents := range map[string]string{
		"v2 unlimited":      "max\n",
		"v1 unlimited":      "9223372036854771712\n",
		"empty file":        "",
		"not a number":      "banana\n",
		"zero":              "0\n",
		"negative":          "-1\n",
	} {
		t.Run(name, func(t *testing.T) {
			writeLimit(t, contents)

			if n, ok := budgetFromMemoryLimit(); ok {
				t.Errorf("derived a budget of %d from %q", n, contents)
			}
		})
	}
}

// A missing file is the ordinary case off Kubernetes, and v1 is read only
// when v2 is absent.
func TestFallsThroughToTheSecondPath(t *testing.T) {
	writeLimit(t, "", "14680064000\n")

	if _, ok := budgetFromMemoryLimit(); !ok {
		t.Fatal("did not fall through to the v1 path")
	}

	writeLimit(t, "", "")

	if _, ok := budgetFromMemoryLimit(); ok {
		t.Fatal("derived a budget with no cgroup files at all")
	}
}

// A container small enough that a quarter of it is derisory still gets a
// usable cache rather than one that thrashes against its own collector.
func TestTinyContainerGetsTheFloor(t *testing.T) {
	writeLimit(t, "134217728\n") // 128 MiB

	got, ok := budgetFromMemoryLimit()
	if !ok {
		t.Fatal("no budget derived")
	}

	if got != cgroupBudgetFloor {
		t.Errorf("budget %d, want the floor %d", got, cgroupBudgetFloor)
	}
}
