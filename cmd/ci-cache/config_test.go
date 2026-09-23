package main

import (
	"context"
	"io"
	"os"
	"reflect"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/truvity/ci-cache/config"
)

// equivalentFlags says, by hand, what testdata/config.yaml says in YAML.
//
// Writing it out twice is the point. The two forms are produced by different
// code -- one decodes a document, the other parses strings off a command line
// -- and the only way to know they agree is to state the same configuration
// in both and compare the results.
var equivalentFlags = []string{
	"--store.bucket=ci-cache-test",
	"--store.region=auto",
	"--store.endpoint=https://example.r2.cloudflarestorage.com",
	"--store.path-style",
	"--store.key-prefix=estate/",

	"--frontends.go.build.enabled=false",
	"--frontends.go.build.legacy-prefix=old/go/build/",
	"--frontends.go.mod.enabled=false",
	"--frontends.go.mod.upstream=https://proxy.example.test",
	"--frontends.go.mod.sumdb=sum.example.test",
	"--frontends.go.mod.list-ttl=45s",
	"--frontends.go.mod.legacy-prefix=old/go/mod/",

	"--frontends.maven.enabled",
	"--frontends.maven.upstreams=central=https://repo1.example.test/maven2," +
		"internal=https://nexus.example.test/repo",
	"--frontends.maven.metadata-ttl=3m",
	"--frontends.maven.negative-ttl=90s",

	"--frontends.gradle.build.enabled",
	"--frontends.gradle.build.read-only",
	"--frontends.gradle.build.max-entry=268435456",
	"--frontends.gradle.dist.enabled",
	"--frontends.gradle.dist.allow=https://services.example.test/distributions/," +
		"https://mirror.example.test/gradle/",

	"--frontends.nix.enabled",
	"--frontends.nix.upstreams=https://cache.example.test",
	"--frontends.nix.priority=5",
	"--frontends.nix.negative-ttl=30s",

	"--frontends.npm.enabled",
	"--frontends.npm.upstream=https://registry.example.test",
	"--frontends.npm.metadata-ttl=2m",

	"--frontends.bazel.enabled",

	"--persistence.dir=/srv/cache",
	"--persistence.ephemeral-is-acceptable",

	"--gc.floor=15",
	"--gc.high=90",
	"--gc.low=70",
	"--gc.budget-bytes=10737418240",
	"--gc.budgets=go=60,npm=20",

	"--upload.concurrency=16",
	"--upload.queue=2048",
	"--upload.queue-bytes=4294967296",
	"--upload.min-size=4096",

	"--service.data=9090",
	"--service.admin=9091",

	"--server.concurrency=512",
	"--server.drain-timeout=1m",

	"--telemetry.otlp-endpoint=http://otel.example.test:4318",
	"--telemetry.trace-ratio=0.25",

	"--log-level=debug",
}

// load runs the resolver the way a subcommand does.
func load(t *testing.T, args ...string) config.Config {
	t.Helper()
	var got config.Config
	var loadErr error
	cmd := &cli.Command{
		Name:      "test",
		Flags:     configFlags(),
		Writer:    io.Discard,
		ErrWriter: io.Discard,
		Action: func(_ context.Context, c *cli.Command) error {
			got, loadErr = loadConfig(c)
			return loadErr
		},
	}
	if err := cmd.Run(context.Background(), append([]string{"test"}, args...)); err != nil {
		t.Fatalf("run %v: %v", args, err)
	}
	return got
}

func TestFileAndFlagsAgree(t *testing.T) {
	fromFile := load(t, "--config", "testdata/config.yaml")
	fromFlags := load(t, equivalentFlags...)

	if !reflect.DeepEqual(fromFile, fromFlags) {
		reportDiff(t, "file", fromFile, "flags", fromFlags)
	}
}

// TestGoldenIsNotTheDefault keeps the comparison above honest.
//
// Two resolvers that both ignored the input would agree perfectly, and the
// test would pass while proving nothing. Every field in the golden file has
// to differ from the default, or it is not being tested at all.
func TestGoldenIsNotTheDefault(t *testing.T) {
	got := load(t, "--config", "testdata/config.yaml")
	def := config.Default()

	gv, dv := reflect.ValueOf(got), reflect.ValueOf(def)
	for _, l := range configLeaves() {
		g, d := gv.FieldByIndex(l.index), dv.FieldByIndex(l.index)
		if reflect.DeepEqual(g.Interface(), d.Interface()) {
			t.Errorf("%s is %v in testdata/config.yaml and in the defaults: it proves nothing",
				l.flag, g.Interface())
		}
	}
}

func TestEnvAliasesResolve(t *testing.T) {
	t.Setenv("CI_CACHE_STORE_BUCKET", "from-env")
	t.Setenv("CI_CACHE_STORE_PATH_STYLE", "true")
	t.Setenv("CI_CACHE_FRONTENDS_GO_MOD_LIST_TTL", "90s")
	t.Setenv("CI_CACHE_GC_BUDGETS", "go=70,nix=10")
	t.Setenv("CI_CACHE_FRONTENDS_NIX_UPSTREAMS", "https://one.test,https://two.test")
	t.Setenv("CI_CACHE_TELEMETRY_TRACE_RATIO", "0.5")
	t.Setenv("CI_CACHE_LOG_LEVEL", "debug")

	got := load(t)

	if got.Store.Bucket != "from-env" {
		t.Errorf("store.bucket = %q, want %q", got.Store.Bucket, "from-env")
	}
	if !got.Store.PathStyle {
		t.Error("store.pathStyle = false, want true")
	}
	if got.Frontends.Go.Mod.ListTTL.String() != "1m30s" {
		t.Errorf("frontends.go.mod.listTTL = %s, want 1m30s", got.Frontends.Go.Mod.ListTTL)
	}
	if want := map[string]int{"go": 70, "nix": 10}; !reflect.DeepEqual(got.GC.Budgets, want) {
		t.Errorf("gc.budgets = %v, want %v", got.GC.Budgets, want)
	}
	if want := []string{"https://one.test", "https://two.test"}; !reflect.DeepEqual(got.Frontends.Nix.Upstreams, want) {
		t.Errorf("frontends.nix.upstreams = %v, want %v", got.Frontends.Nix.Upstreams, want)
	}
	if got.Telemetry.TraceRatio != 0.5 {
		t.Errorf("telemetry.traceRatio = %v, want 0.5", got.Telemetry.TraceRatio)
	}
	if got.LogLevel != "debug" {
		t.Errorf("logLevel = %q, want debug", got.LogLevel)
	}
}

// TestPrecedence pins the order the chart depends on: a flag beats the
// environment, the environment beats the file, and the file beats the
// defaults. Getting this wrong is not a visible bug -- it is a setting that
// quietly does not apply, on whichever of the three a particular estate uses.
func TestPrecedence(t *testing.T) {
	t.Setenv("CI_CACHE_STORE_BUCKET", "from-env")
	t.Setenv("CI_CACHE_SERVICE_DATA", "7002")
	t.Setenv("CI_CACHE_FRONTENDS_GO_BUILD_ENABLED", "false")

	t.Run("flag over env over file", func(t *testing.T) {
		got := load(t, "--config", "testdata/config.yaml", "--store.bucket=from-flag")
		if got.Store.Bucket != "from-flag" {
			t.Errorf("store.bucket = %q, want from-flag", got.Store.Bucket)
		}
		if got.Service.Data != 7002 {
			t.Errorf("service.data = %d, want 7002 from the environment", got.Service.Data)
		}
		// The file says false as well; what matters is that a bool set in the
		// environment is applied at all, since its zero value is also false.
		if got.Frontends.Go.Build.Enabled {
			t.Error("frontends.go.build.enabled = true, want false")
		}
		// Untouched by flag or environment: the file still wins over the
		// default.
		if got.Persistence.Dir != "/srv/cache" {
			t.Errorf("persistence.dir = %q, want /srv/cache from the file", got.Persistence.Dir)
		}
	})

	t.Run("env over file", func(t *testing.T) {
		got := load(t, "--config", "testdata/config.yaml")
		if got.Store.Bucket != "from-env" {
			t.Errorf("store.bucket = %q, want from-env", got.Store.Bucket)
		}
	})

	t.Run("file over default", func(t *testing.T) {
		t.Setenv("CI_CACHE_STORE_BUCKET", "")
		got := load(t, "--config", "testdata/config.yaml")
		// An empty environment variable is still set, and setting a value to
		// nothing is a thing somebody may mean.
		if got.Store.Bucket != "" {
			t.Errorf("store.bucket = %q, want the empty value the environment asked for", got.Store.Bucket)
		}
		if got.Server.DrainTimeout.String() != "1m0s" {
			t.Errorf("server.drainTimeout = %s, want 1m0s from the file", got.Server.DrainTimeout)
		}
	})
}

// TestEnvAliasNames pins the names the Helm chart writes into a container's
// environment. Renaming one is a breaking change for every deployment, so it
// is spelled out here rather than derived, which is the only way a rename
// shows up as a failing test instead of a cache that ignores its settings.
func TestEnvAliasNames(t *testing.T) {
	want := map[string]string{
		"store.bucket":                        "CI_CACHE_STORE_BUCKET",
		"store.path-style":                    "CI_CACHE_STORE_PATH_STYLE",
		"store.key-prefix":                    "CI_CACHE_STORE_KEY_PREFIX",
		"frontends.go.build.enabled":          "CI_CACHE_FRONTENDS_GO_BUILD_ENABLED",
		"frontends.go.mod.list-ttl":           "CI_CACHE_FRONTENDS_GO_MOD_LIST_TTL",
		"frontends.go.mod.sumdb":              "CI_CACHE_FRONTENDS_GO_MOD_SUMDB",
		"frontends.maven.upstreams":           "CI_CACHE_FRONTENDS_MAVEN_UPSTREAMS",
		"frontends.gradle.build.max-entry":    "CI_CACHE_FRONTENDS_GRADLE_BUILD_MAX_ENTRY",
		"frontends.gradle.dist.allow":         "CI_CACHE_FRONTENDS_GRADLE_DIST_ALLOW",
		"frontends.nix.negative-ttl":          "CI_CACHE_FRONTENDS_NIX_NEGATIVE_TTL",
		"frontends.npm.metadata-ttl":          "CI_CACHE_FRONTENDS_NPM_METADATA_TTL",
		"frontends.bazel.enabled":             "CI_CACHE_FRONTENDS_BAZEL_ENABLED",
		"persistence.ephemeral-is-acceptable": "CI_CACHE_PERSISTENCE_EPHEMERAL_IS_ACCEPTABLE",
		"gc.budget-bytes":                     "CI_CACHE_GC_BUDGET_BYTES",
		"upload.queue-bytes":                  "CI_CACHE_UPLOAD_QUEUE_BYTES",
		"service.admin":                       "CI_CACHE_SERVICE_ADMIN",
		"server.drain-timeout":                "CI_CACHE_SERVER_DRAIN_TIMEOUT",
		"telemetry.otlp-endpoint":             "CI_CACHE_TELEMETRY_OTLP_ENDPOINT",
		"log-level":                           "CI_CACHE_LOG_LEVEL",
	}

	got := make(map[string]string, len(configLeaves()))
	for _, l := range configLeaves() {
		got[l.flag] = l.env
	}
	for flag, env := range want {
		if got[flag] != env {
			t.Errorf("flag --%s has env alias %q, want %q", flag, got[flag], env)
		}
	}
}

// TestUnknownKeyIsRefused: a key the schema does not have is almost always a
// typo in somebody's values file, and accepting it silently is how a setting
// spends a release doing nothing.
func TestUnknownKeyIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/bad.yaml"
	if err := os.WriteFile(path, []byte("store:\n  buckett: typo\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var loadErr error
	cmd := &cli.Command{
		Name:      "test",
		Flags:     configFlags(),
		Writer:    io.Discard,
		ErrWriter: io.Discard,
		Action: func(_ context.Context, c *cli.Command) error {
			_, loadErr = loadConfig(c)
			return nil
		},
	}
	if err := cmd.Run(context.Background(), []string{"test", "--config", path}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if loadErr == nil {
		t.Fatal("a misspelled key was accepted")
	}
}

func reportDiff(t *testing.T, aName string, a config.Config, bName string, b config.Config) {
	t.Helper()
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	for _, l := range configLeaves() {
		x, y := av.FieldByIndex(l.index).Interface(), bv.FieldByIndex(l.index).Interface()
		if !reflect.DeepEqual(x, y) {
			t.Errorf("%s: %s = %#v, %s = %#v", l.flag, aName, x, bName, y)
		}
	}
}
