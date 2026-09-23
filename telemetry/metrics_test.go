package telemetry

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestMetricNamesMatchGolden is the gate the package comment promises.
//
// The golden file is what the dashboard graphs and what the alert rules
// select. Adding an instrument without updating it is a red mark, so that the
// dashboard change is reviewed in the same diff rather than in whatever
// quarter somebody notices a panel is empty. Renaming one is the same red
// mark for the same reason, plus a query that has silently returned no data
// since the rename.
func TestMetricNamesMatchGolden(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("testdata/metric_names.golden")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	want := strings.Fields(string(raw))

	if len(MetricNames) != len(want) {
		t.Fatalf("MetricNames has %d entries, golden has %d:\n got %v\nwant %v",
			len(MetricNames), len(want), MetricNames, want)
	}
	for i := range want {
		if MetricNames[i] != want[i] {
			t.Errorf("MetricNames[%d] = %q, golden has %q "+
				"(update testdata/metric_names.golden AND the dashboard)", i, MetricNames[i], want[i])
		}
	}
}

// TestMetricNamesIsSortedAndUnique keeps the golden file diffable.
//
// An unsorted list makes every addition a diff that touches an arbitrary
// line, and a duplicate makes the count assertion above pass while one
// instrument is missing.
func TestMetricNamesIsSortedAndUnique(t *testing.T) {
	t.Parallel()

	if !sort.StringsAreSorted(MetricNames) {
		t.Errorf("MetricNames is not sorted: %v", MetricNames)
	}
	seen := map[string]bool{}
	for _, n := range MetricNames {
		if seen[n] {
			t.Errorf("MetricNames contains %q twice", n)
		}
		seen[n] = true
	}
}

// constPattern finds the instrument-name constants in the source.
var constPattern = regexp.MustCompile(`(?m)^\tMetric[A-Za-z]+ = "(cicache\.[a-z._]+)"$`)

// TestEveryMetricConstantIsListed is the half of the gate the golden file
// cannot cover on its own.
//
// Declaring a constant and forgetting to add it to MetricNames would leave
// the golden file passing while an instrument nobody graphs is exported. The
// source is read rather than reflected over because Go has no way to
// enumerate a package's constants at run time.
func TestEveryMetricConstantIsListed(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("metrics.go")
	if err != nil {
		t.Fatalf("read metrics.go: %v", err)
	}
	matches := constPattern.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		// A guard that scanned nothing is a guard that passes forever: if the
		// constants move or change shape, this must fail rather than agree.
		t.Fatalf("found no instrument constants in metrics.go: the pattern %q no longer matches the source",
			constPattern)
	}

	listed := map[string]bool{}
	for _, n := range MetricNames {
		listed[n] = true
	}
	for _, m := range matches {
		if !listed[m[1]] {
			t.Errorf("metrics.go declares %q but MetricNames does not list it", m[1])
		}
	}
	if len(matches) != len(MetricNames) {
		t.Errorf("metrics.go declares %d instruments, MetricNames lists %d", len(matches), len(MetricNames))
	}
}
