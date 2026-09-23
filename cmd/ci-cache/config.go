package main

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"

	"github.com/truvity/ci-cache/config"
)

// The configuration surface is derived from config.Config rather than written
// out beside it.
//
// There is exactly one schema, and a chart, a file and a flag are three ways
// of saying the same thing. Listing the fields a second time here would mean
// that adding one to the schema and forgetting to add it here produces a
// binary that silently ignores what the chart set -- which is the failure
// mode this whole arrangement exists to prevent. Walking the struct makes
// that impossible: a field that exists is a flag, an environment alias and a
// YAML key, or it is none of them.

// envPrefix is fixed. The chart writes these names into a container's
// environment, so changing it is a breaking change for every deployment.
const envPrefix = "CI_CACHE_"

// configFlagName is the flag that names a YAML file of the same shape.
const configFlagName = "config"

// leaf is one settable value in the schema.
type leaf struct {
	index []int    // field index path into config.Config
	yaml  []string // yaml key path, e.g. {"frontends","go","mod","listTTL"}
	flag  string   // "frontends.go.mod.list-ttl"
	env   string   // "CI_CACHE_FRONTENDS_GO_MOD_LIST_TTL"
	typ   reflect.Type
}

var (
	leavesOnce sync.Once
	leavesAll  []leaf
)

// configLeaves walks config.Config once and caches the result.
func configLeaves() []leaf {
	leavesOnce.Do(func() {
		leavesAll = walk(reflect.TypeOf(config.Config{}), nil, nil)
	})
	return leavesAll
}

var durationType = reflect.TypeOf(time.Duration(0))

func walk(t reflect.Type, index []int, path []string) []leaf {
	var out []leaf
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		idx := append(append([]int{}, index...), i)
		p := append(append([]string{}, path...), name)

		// A struct is a section; anything else -- including a duration,
		// which is an int64 wearing a name -- is a value.
		if f.Type.Kind() == reflect.Struct && f.Type != durationType {
			out = append(out, walk(f.Type, idx, p)...)
			continue
		}
		out = append(out, leaf{
			index: idx,
			yaml:  p,
			flag:  flagName(p),
			env:   envName(p),
			typ:   f.Type,
		})
	}
	return out
}

// flagName is the YAML path with each segment kebab-cased, joined by dots:
// the dots keep the flag readable as the path it is, and the kebab keeps each
// segment readable as the words it is made of.
func flagName(path []string) string {
	parts := make([]string, 0, len(path))
	for _, p := range path {
		parts = append(parts, strings.Join(words(p), "-"))
	}
	return strings.Join(parts, ".")
}

// envName is the same path in upper snake case under the fixed prefix.
func envName(path []string) string {
	var parts []string
	for _, p := range path {
		parts = append(parts, words(p)...)
	}
	return envPrefix + strings.ToUpper(strings.Join(parts, "_"))
}

// words splits a camelCase or lowercase identifier into its words, keeping
// an acronym whole: "listTTL" is list and TTL, not list, T, T and L, so the
// environment alias reads CI_CACHE_..._LIST_TTL and not ..._LIST_T_T_L.
func words(s string) []string {
	runes := []rune(s)
	var out []string
	start := 0
	for i := 1; i < len(runes); i++ {
		prev, cur := runes[i-1], runes[i]
		boundary := isLower(prev) && isUpper(cur)
		if !boundary && isUpper(prev) && isUpper(cur) && i+1 < len(runes) && isLower(runes[i+1]) {
			boundary = true
		}
		if boundary {
			out = append(out, strings.ToLower(string(runes[start:i])))
			start = i
		}
	}
	return append(out, strings.ToLower(string(runes[start:])))
}

func isUpper(r rune) bool { return r >= 'A' && r <= 'Z' }
func isLower(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') }

// configFlags returns the flags for the whole schema, or for the sections
// whose YAML path starts with one of the given prefixes.
//
// A subcommand that only needs the object store should not print sixty flags
// in its help; the environment aliases still resolve for all of them, because
// an environment is inherited whether or not a flag was declared.
func configFlags(sections ...string) []cli.Flag {
	flags := []cli.Flag{
		&cli.StringFlag{
			Name:  configFlagName,
			Usage: "read configuration from a YAML file of the same shape [$" + envPrefix + "CONFIG]",
		},
	}
	for _, l := range configLeaves() {
		if !inSections(l, sections) {
			continue
		}
		usage := fmt.Sprintf("%s [$%s]", strings.Join(l.yaml, "."), l.env)
		if l.typ.Kind() == reflect.Bool {
			// Bools are real switches rather than strings that happen to
			// parse: --frontends.go.build.enabled and its =false form are
			// what anyone writing these by hand expects.
			flags = append(flags, &cli.BoolFlag{Name: l.flag, Usage: usage})
			continue
		}
		flags = append(flags, &cli.StringFlag{Name: l.flag, Usage: usage})
	}
	return flags
}

func inSections(l leaf, sections []string) bool {
	if len(sections) == 0 {
		return true
	}
	for _, s := range sections {
		if l.yaml[0] == s {
			return true
		}
	}
	return false
}

// loadConfig resolves the configuration in one place, in one order.
//
// Precedence is flag, then environment, then file, then the schema's own
// defaults, and it is applied by layering rather than by asking a flag
// library what it thinks: urfave resolves an environment source INTO a flag,
// which makes "set by a flag" and "set by the environment" indistinguishable
// and the ordering above unenforceable.
func loadConfig(cmd *cli.Command) (config.Config, error) {
	cfg := config.Default()

	path := cmd.String(configFlagName)
	if path == "" {
		path = os.Getenv(envPrefix + "CONFIG")
	}
	if path != "" {
		if err := loadConfigFile(path, &cfg); err != nil {
			return cfg, err
		}
	}

	v := reflect.ValueOf(&cfg).Elem()
	for _, l := range configLeaves() {
		if s, ok := os.LookupEnv(l.env); ok {
			if err := setLeaf(v.FieldByIndex(l.index), s); err != nil {
				return cfg, fmt.Errorf("%s=%q: %w", l.env, s, err)
			}
		}
	}
	for _, l := range configLeaves() {
		if !cmd.IsSet(l.flag) {
			continue
		}
		s := cmd.String(l.flag)
		if l.typ.Kind() == reflect.Bool {
			s = strconv.FormatBool(cmd.Bool(l.flag))
		}
		if err := setLeaf(v.FieldByIndex(l.index), s); err != nil {
			return cfg, fmt.Errorf("--%s=%q: %w", l.flag, s, err)
		}
	}
	return cfg, nil
}

// loadConfigFile decodes YAML over the defaults, refusing a key the schema
// does not have.
//
// A silently ignored key is how a chart ends up describing a cache that is
// not the cache running: the value renders, the container starts, and the
// setting does nothing for however long it takes somebody to measure it.
func loadConfigFile(path string, cfg *config.Config) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", configFlagName, err)
	}
	defer func() { _ = f.Close() }()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// setLeaf parses one value into one field.
//
// Flags and environment variables go through this same function, which is
// what makes the two indistinguishable in the result: a value that means one
// thing on the command line and another in a container's environment is a
// bug that only ever shows up in production.
func setLeaf(v reflect.Value, s string) error {
	if v.Type() == durationType {
		d, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		v.SetInt(int64(d))
		return nil
	}

	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		v.SetFloat(f)
	case reflect.Slice:
		return setSlice(v, s)
	case reflect.Map:
		return setMap(v, s)
	default:
		return fmt.Errorf("unsupported configuration type %s", v.Type())
	}
	return nil
}

// setSlice reads a comma-separated list. Empty means an empty list and not
// "leave whatever was there": a caller that says --frontends.nix.upstreams=
// is removing the default, which is a thing an estate with its own mirror
// wants to do.
func setSlice(v reflect.Value, s string) error {
	if v.Type().Elem().Kind() != reflect.String {
		return fmt.Errorf("unsupported configuration type %s", v.Type())
	}
	out := reflect.MakeSlice(v.Type(), 0, 0)
	for _, part := range splitList(s) {
		out = reflect.Append(out, reflect.ValueOf(part))
	}
	v.Set(out)
	return nil
}

// setMap reads a comma-separated list of key=value pairs.
func setMap(v reflect.Value, s string) error {
	t := v.Type()
	if t.Key().Kind() != reflect.String {
		return fmt.Errorf("unsupported configuration type %s", t)
	}
	out := reflect.MakeMap(t)
	for _, part := range splitList(s) {
		k, val, ok := strings.Cut(part, "=")
		if !ok {
			return fmt.Errorf("%q is not key=value", part)
		}
		ev := reflect.New(t.Elem()).Elem()
		if err := setLeaf(ev, strings.TrimSpace(val)); err != nil {
			return fmt.Errorf("key %q: %w", k, err)
		}
		out.SetMapIndex(reflect.ValueOf(strings.TrimSpace(k)), ev)
	}
	v.Set(out)
	return nil
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
