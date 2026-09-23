// Package config is the server's one configuration schema.
//
// The same shape is a YAML file, a set of flags and a set of environment
// variables, and it is the shape the Helm chart's values take. One schema
// means the chart, the CLI and the deploy pages cannot describe different
// things -- which they will, the moment there are two.
//
// Environment aliases are CI_CACHE_<SECTION>_<NAME>, upper snake case.
package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Config is everything `ci-cache serve` needs.
type Config struct {
	Store       Store       `yaml:"store"`
	Frontends   Frontends   `yaml:"frontends"`
	Persistence Persistence `yaml:"persistence"`
	GC          GC          `yaml:"gc"`
	Upload      Upload      `yaml:"upload"`
	Service     Service     `yaml:"service"`
	Server      Server      `yaml:"server"`
	Telemetry   Telemetry   `yaml:"telemetry"`
	LogLevel    string      `yaml:"logLevel"`
}

// Store is the object store behind the disk: AWS S3, Cloudflare R2, or
// anything else that speaks the S3 API.
type Store struct {
	Bucket string `yaml:"bucket"`
	// Region is required. The SDK's own resolution inside a pod is whatever
	// the environment happens to carry, and R2 rejects a bucket-location
	// lookup outright -- it answers to "auto".
	Region string `yaml:"region"`
	// Endpoint empty keeps AWS. Setting it also turns off the SDK's default
	// request and response checksums, which stores other than AWS reject.
	Endpoint string `yaml:"endpoint"`
	// PathStyle addresses the bucket as endpoint/bucket/key. It is a property
	// of the store's certificate -- does its wildcard cover a bucket
	// subdomain? -- not of the endpoint, so it is its own switch.
	PathStyle bool `yaml:"pathStyle"`
	// KeyPrefix goes in front of every key. Empty is the natural layout
	// (go/build/..., nix/...); a value is for a bucket that already holds
	// something else.
	KeyPrefix string `yaml:"keyPrefix"`
}

// Frontends is which protocols this installation serves.
type Frontends struct {
	Go     GoFrontend     `yaml:"go"`
	Maven  MavenFrontend  `yaml:"maven"`
	Gradle GradleFrontend `yaml:"gradle"`
	Nix    NixFrontend    `yaml:"nix"`
	NPM    NPMFrontend    `yaml:"npm"`
	Bazel  BazelFrontend  `yaml:"bazel"`
}

// GoFrontend is the two Go caches: compiled actions and downloaded modules.
type GoFrontend struct {
	Build GoBuild `yaml:"build"`
	Mod   GoMod   `yaml:"mod"`
}

// GoBuild is the Go build cache, reached by the runner agent over Connect.
type GoBuild struct {
	Enabled bool `yaml:"enabled"`
	// LegacyPrefix is consulted on a bucket miss. A bucket already warmed by
	// go-cache-plugin keeps serving through the switch; writes always go to
	// the new layout.
	LegacyPrefix string `yaml:"legacyPrefix"`
}

// GoMod is the Go module and sumdb proxy.
type GoMod struct {
	Enabled      bool          `yaml:"enabled"`
	Upstream     string        `yaml:"upstream"`
	SumDB        string        `yaml:"sumdb"`
	ListTTL      time.Duration `yaml:"listTTL"`
	LegacyPrefix string        `yaml:"legacyPrefix"`
}

// MavenFrontend proxies one or more Maven repositories, each at its own path.
type MavenFrontend struct {
	Enabled bool `yaml:"enabled"`
	// Upstreams is name -> base URL; each becomes /maven/<name>.
	Upstreams   map[string]string `yaml:"upstreams"`
	MetadataTTL time.Duration     `yaml:"metadataTTL"`
	NegativeTTL time.Duration     `yaml:"negativeTTL"`
}

// GradleFrontend is Gradle's two needs: its remote build cache, and the
// wrapper distribution every cold job downloads.
type GradleFrontend struct {
	Build GradleBuild `yaml:"build"`
	Dist  GradleDist  `yaml:"dist"`
}

// GradleBuild is Gradle's remote HTTP build cache, which needs no agent:
// Gradle speaks it natively.
type GradleBuild struct {
	Enabled bool `yaml:"enabled"`
	// ReadOnly refuses PUT, for an estate where only CI may fill the cache.
	ReadOnly bool  `yaml:"readOnly"`
	MaxEntry int64 `yaml:"maxEntry"`
}

// GradleDist serves wrapper distributions from an allow list.
type GradleDist struct {
	Enabled bool `yaml:"enabled"`
	// Allow is the list of upstream prefixes this may fetch. Anything else
	// is refused: a proxy with no allow-list is an open relay.
	Allow []string `yaml:"allow"`
}

// NixFrontend is a nix binary cache standing in front of the upstreams, as a
// substituter rather than a proxy -- so no TLS is intercepted.
type NixFrontend struct {
	Enabled   bool     `yaml:"enabled"`
	Upstreams []string `yaml:"upstreams"`
	// Priority is what nix-cache-info advertises. Nix prefers the LOWEST, and
	// cache.nixos.org says 40, so anything below that wins.
	Priority    int           `yaml:"priority"`
	NegativeTTL time.Duration `yaml:"negativeTTL"`
}

// NPMFrontend proxies an npm registry.
type NPMFrontend struct {
	Enabled     bool          `yaml:"enabled"`
	Upstream    string        `yaml:"upstream"`
	MetadataTTL time.Duration `yaml:"metadataTTL"`
}

// BazelFrontend serves the cache half of the Bazel Remote Execution API,
// which is what moon's task cache speaks.
type BazelFrontend struct {
	Enabled bool `yaml:"enabled"`
}

// Persistence is the disk tier's volume.
type Persistence struct {
	Dir string `yaml:"dir"`
	// EphemeralIsAcceptable acknowledges that the cache is on a volume that
	// does not survive a restart. Without it a build-cache front-end on an
	// emptyDir is refused: it is the shape that looks like a cache and keeps
	// nothing, which is what this whole service exists to stop.
	EphemeralIsAcceptable bool `yaml:"ephemeralIsAcceptable"`
}

// GC is how the disk tier is kept inside its volume.
type GC struct {
	// Floor is the fraction of the filesystem left free, as a percentage.
	// The budget is derived from statfs, so resizing the volume needs no
	// other change.
	Floor int `yaml:"floor"`
	// High and Low are percentages of the budget: eviction starts at High
	// and runs until Low.
	High int `yaml:"high"`
	Low  int `yaml:"low"`
	// BudgetBytes overrides the derived budget, for a volume shared with
	// something else.
	BudgetBytes int64 `yaml:"budgetBytes"`
	// Budgets caps one front-end, as a percentage of the whole. A front-end
	// over its cap is evicted before anything else.
	Budgets map[string]int `yaml:"budgets"`
}

// Upload is the write-behind queue in front of the bucket.
type Upload struct {
	Concurrency int   `yaml:"concurrency"`
	Queue       int   `yaml:"queue"`
	QueueBytes  int64 `yaml:"queueBytes"`
	// MinSize skips the bucket for objects below it: a round trip per
	// hundred-byte object costs more than re-deriving it.
	MinSize int64 `yaml:"minSize"`
}

// Service is the two listeners.
type Service struct {
	// Data carries every front-end and is reachable by every CI job.
	Data int `yaml:"data"`
	// Admin carries Stats, List, the wipes and the UI. It is never in a
	// consumer NetworkPolicy: a job that can wipe can empty the cache for
	// everyone.
	Admin int `yaml:"admin"`
}

// Server is the data listener's own limits.
type Server struct {
	Concurrency  int           `yaml:"concurrency"`
	DrainTimeout time.Duration `yaml:"drainTimeout"`
}

// Telemetry is where metrics and traces go. Empty exports nothing and still
// counts everything in process.
type Telemetry struct {
	OTLPEndpoint string  `yaml:"otlpEndpoint"`
	TraceRatio   float64 `yaml:"traceRatio"`
}

// Default is the configuration before anything is said about it. Every value
// here is safe on any estate; what is left empty is what an estate must
// decide -- the bucket above all.
func Default() Config {
	return Config{
		Store: Store{Region: "", KeyPrefix: ""},
		Frontends: Frontends{
			Go: GoFrontend{
				Build: GoBuild{Enabled: true},
				Mod: GoMod{
					Enabled:  true,
					Upstream: "https://proxy.golang.org",
					SumDB:    "sum.golang.org",
					ListTTL:  10 * time.Minute,
				},
			},
			Maven: MavenFrontend{MetadataTTL: 10 * time.Minute, NegativeTTL: 5 * time.Minute},
			Gradle: GradleFrontend{
				Build: GradleBuild{MaxEntry: 512 << 20},
				Dist:  GradleDist{Allow: []string{"https://services.gradle.org/distributions/"}},
			},
			Nix: NixFrontend{
				Upstreams:   []string{"https://cache.nixos.org"},
				Priority:    10,
				NegativeTTL: 5 * time.Minute,
			},
			NPM: NPMFrontend{Upstream: "https://registry.npmjs.org", MetadataTTL: 5 * time.Minute},
		},
		Persistence: Persistence{Dir: "/data"},
		GC:          GC{Floor: 10, High: 95, Low: 85},
		Upload:      Upload{Concurrency: 8, Queue: 1024, QueueBytes: 2 << 30},
		Service:     Service{Data: 8080, Admin: 8081},
		Server:      Server{Concurrency: 256, DrainTimeout: 30 * time.Second},
		Telemetry:   Telemetry{TraceRatio: 0.01},
		LogLevel:    "info",
	}
}

// Validate refuses a configuration that would deploy and then misbehave.
//
// Every rule here is one the chart also refuses at render time. Two gates for
// one rule is not duplication: the chart catches it before a cluster sees it,
// and this catches it when somebody runs the binary by hand.
func (c *Config) Validate() error {
	if c.Store.Bucket == "" {
		return fmt.Errorf("store.bucket is required")
	}
	if c.Store.Region == "" {
		return fmt.Errorf("store.region is required (use \"auto\" on Cloudflare R2)")
	}
	if c.Store.Endpoint != "" {
		u, err := url.Parse(c.Store.Endpoint)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("store.endpoint %q is not an absolute URL", c.Store.Endpoint)
		}
	}
	if c.Persistence.Dir == "" {
		return fmt.Errorf("persistence.dir is required")
	}
	if c.Frontends.Go.Build.Enabled && !c.Persistence.EphemeralIsAcceptable && isEphemeral(c.Persistence.Dir) {
		return fmt.Errorf("persistence.dir %q looks ephemeral and the build cache is on: "+
			"set persistence.ephemeralIsAcceptable to say that is intended", c.Persistence.Dir)
	}
	if c.GC.Floor < 0 || c.GC.Floor > 90 {
		return fmt.Errorf("gc.floor must be between 0 and 90, got %d", c.GC.Floor)
	}
	if c.GC.Low >= c.GC.High {
		return fmt.Errorf("gc.low (%d) must be below gc.high (%d)", c.GC.Low, c.GC.High)
	}
	if c.GC.High > 100 {
		return fmt.Errorf("gc.high must be at most 100, got %d", c.GC.High)
	}
	if c.Service.Data == c.Service.Admin {
		return fmt.Errorf("service.data and service.admin must differ: the admin port is a boundary, not a path")
	}
	if c.Frontends.Maven.Enabled && len(c.Frontends.Maven.Upstreams) == 0 {
		return fmt.Errorf("frontends.maven.enabled needs at least one upstream")
	}
	for name, u := range c.Frontends.Maven.Upstreams {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return fmt.Errorf("frontends.maven.upstreams[%s] must be an absolute URL, got %q", name, u)
		}
	}
	if c.Frontends.Gradle.Dist.Enabled && len(c.Frontends.Gradle.Dist.Allow) == 0 {
		return fmt.Errorf("frontends.gradle.dist.enabled needs an allow list: " +
			"a distribution proxy with no allow list is an open relay")
	}
	if c.Frontends.Nix.Enabled && len(c.Frontends.Nix.Upstreams) == 0 {
		return fmt.Errorf("frontends.nix.enabled needs at least one upstream")
	}
	return nil
}

// isEphemeral recognises the shapes a Kubernetes emptyDir takes. It is a
// heuristic and says so: the point is to catch the accident, not to prove a
// filesystem's lifetime, which nothing in the pod can do.
func isEphemeral(dir string) bool {
	return strings.HasPrefix(dir, "/tmp") ||
		strings.HasPrefix(dir, "/var/tmp") ||
		strings.HasPrefix(dir, "/dev/shm")
}
