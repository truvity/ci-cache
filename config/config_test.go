package config_test

import (
	"strings"
	"testing"

	"github.com/truvity/ci-cache/config"
)

// The default configuration must be one edit away from working: a bucket and
// a region. Everything else that could be wrong should already be right, so
// that a first install fails for one reason and not five.
func TestDefaultNeedsOnlyABucketAndARegion(t *testing.T) {
	t.Parallel()

	c := config.Default()
	if err := c.Validate(); err == nil {
		t.Fatal("the default configuration validated with no bucket")
	}

	c.Store.Bucket = "cache"
	c.Store.Region = "auto"
	if err := c.Validate(); err != nil {
		t.Fatalf("the default configuration plus a bucket and region: %v", err)
	}
}

// Every rule here is one the chart also refuses at render time. Two gates for
// one rule is not duplication -- the chart catches it before a cluster sees
// it, and this catches it when somebody runs the binary by hand -- but they
// have to agree, and this is the list they agree on.
func TestValidateRefusesWhatWouldMisbehave(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		mutate func(*config.Config)
		says   string
	}{
		{
			name:   "no bucket",
			mutate: func(c *config.Config) { c.Store.Bucket = "" },
			says:   "store.bucket",
		},
		{
			name:   "no region",
			mutate: func(c *config.Config) { c.Store.Region = "" },
			says:   "store.region",
		},
		{
			name:   "an endpoint that is not a URL",
			mutate: func(c *config.Config) { c.Store.Endpoint = "r2.example.com" },
			says:   "absolute URL",
		},
		{
			// The shape that looks like a cache and keeps nothing, which is
			// the whole reason this service exists.
			name: "a build cache on an ephemeral volume",
			mutate: func(c *config.Config) {
				c.Persistence.Dir = "/tmp/cache"
				c.Frontends.Go.Build.Enabled = true
			},
			says: "ephemeralIsAcceptable",
		},
		{
			name:   "no cache directory",
			mutate: func(c *config.Config) { c.Persistence.Dir = "" },
			says:   "persistence.dir",
		},
		{
			name:   "a floor that leaves no volume",
			mutate: func(c *config.Config) { c.GC.Floor = 95 },
			says:   "gc.floor",
		},
		{
			name:   "watermarks the wrong way round",
			mutate: func(c *config.Config) { c.GC.Low, c.GC.High = 95, 85 },
			says:   "gc.low",
		},
		{
			name:   "a high watermark above the budget",
			mutate: func(c *config.Config) { c.GC.High = 120 },
			says:   "gc.high",
		},
		{
			// The admin port is a boundary, and a boundary that shares a
			// number with the data port is not one.
			name:   "one port for data and admin",
			mutate: func(c *config.Config) { c.Service.Admin = c.Service.Data },
			says:   "must differ",
		},
		{
			name: "maven with no upstream",
			mutate: func(c *config.Config) {
				c.Frontends.Maven.Enabled = true
				c.Frontends.Maven.Upstreams = nil
			},
			says: "upstream",
		},
		{
			name: "a maven upstream that is not a URL",
			mutate: func(c *config.Config) {
				c.Frontends.Maven.Enabled = true
				c.Frontends.Maven.Upstreams = map[string]string{"central": "repo1.maven.org"}
			},
			says: "absolute URL",
		},
		{
			// A distribution proxy with no allow list is an open relay.
			name: "gradle distributions with no allow list",
			mutate: func(c *config.Config) {
				c.Frontends.Gradle.Dist.Enabled = true
				c.Frontends.Gradle.Dist.Allow = nil
			},
			says: "allow list",
		},
		{
			name: "nix with no upstream",
			mutate: func(c *config.Config) {
				c.Frontends.Nix.Enabled = true
				c.Frontends.Nix.Upstreams = nil
			},
			says: "upstream",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := config.Default()
			c.Store.Bucket, c.Store.Region = "cache", "auto"
			tc.mutate(&c)

			err := c.Validate()
			if err == nil {
				t.Fatal("accepted a configuration that would misbehave")
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the message does not name %q: %v", tc.says, err)
			}
		})
	}
}

// Saying so is the whole point of the flag: an operator who means it should
// not have to argue with the binary every start-up.
func TestEphemeralIsAcceptedWhenItIsSaid(t *testing.T) {
	t.Parallel()

	c := config.Default()
	c.Store.Bucket, c.Store.Region = "cache", "auto"
	c.Persistence.Dir = "/tmp/cache"
	c.Persistence.EphemeralIsAcceptable = true

	if err := c.Validate(); err != nil {
		t.Fatalf("an acknowledged ephemeral volume was still refused: %v", err)
	}
}

// The ephemeral check only matters for the build cache. A module proxy on a
// scratch volume is slow on the first request and correct on every one, so
// refusing it would be the binary being clever at an operator's expense.
func TestAnEphemeralVolumeIsFineWithoutTheBuildCache(t *testing.T) {
	t.Parallel()

	c := config.Default()
	c.Store.Bucket, c.Store.Region = "cache", "auto"
	c.Persistence.Dir = "/tmp/cache"
	c.Frontends.Go.Build.Enabled = false

	if err := c.Validate(); err != nil {
		t.Fatalf("a module-only cache on a scratch volume was refused: %v", err)
	}
}
