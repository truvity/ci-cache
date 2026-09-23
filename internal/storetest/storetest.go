// Package storetest is the one check in this repository that talks to a real
// object store, and the reason it exists is that nothing else can catch the
// bug it is aimed at.
//
// The bucket tier's requests were once wrong in a way no unit test and no
// emulator would have shown. Asking the SDK for a checksum ALGORITHM rather
// than supplying the sum makes it choose the framing, and for an object that
// also carries a Content-Encoding it chooses the aws-chunked trailer; the
// signature then covers a payload literal instead of the bytes, and
// Cloudflare R2 answers 403 SignatureDoesNotMatch. An emulator would have
// accepted that request happily, because an emulator accepts what the SDK
// sends -- and what the SDK sent was the bug. The failure lived between a
// real signer and a real store, which is the one place a local fake cannot
// stand in for.
//
// So this test runs against a bucket somebody pays for, and skips loudly
// everywhere else. The unit tests in engine/bucket assert the shape of the
// request and are what CI relies on day to day; this is what says the shape
// is the right one. Run it after any change to how a request is built, and
// before believing that a change to the SDK version is inert:
//
//	CI_CACHE_STORE_BUCKET=ci-cache-scratch \
//	CI_CACHE_STORE_ENDPOINT=https://<account>.r2.cloudflarestorage.com \
//	CI_CACHE_STORE_REGION=auto \
//	go test ./internal/storetest/...
//
// The object it writes is not a convenient one. It is zstd bytes under a key
// carrying '=' and '@', with metadata -- the shape a Go build cache entry and
// a module zip actually take, and the shape that failed. A probe that writes
// "hello" to "test" proves that the credentials work and nothing else.
package storetest

import (
	"fmt"
	"os"
	"strconv"

	"github.com/truvity/ci-cache/config"
)

// Environment variables that name the store. They are deliberately not the
// server's own CI_CACHE_STORE_* configuration aliases' job: this is a test
// pointing at a scratch bucket, and it must never be satisfied by whatever a
// deployment happens to have in the environment of the shell that ran it.
const (
	EnvBucket    = "CI_CACHE_STORE_BUCKET"
	EnvEndpoint  = "CI_CACHE_STORE_ENDPOINT"
	EnvRegion    = "CI_CACHE_STORE_REGION"
	EnvPathStyle = "CI_CACHE_STORE_PATH_STYLE"
	EnvAWS       = "CI_CACHE_STORE_AWS"
)

// FromEnv reports the store to test against.
//
// ok is false when the environment names no bucket, which is the ordinary
// case and a skip rather than a failure. An error is a half-configured
// environment -- a bucket with neither an endpoint nor CI_CACHE_STORE_AWS,
// say -- which is somebody trying to run this and getting it wrong, and must
// not be silently skipped.
func FromEnv() (cfg config.Store, ok bool, err error) {
	bucket := os.Getenv(EnvBucket)
	if bucket == "" {
		return config.Store{}, false, nil
	}
	cfg = config.Store{Bucket: bucket, Region: os.Getenv(EnvRegion)}

	switch {
	case os.Getenv(EnvEndpoint) != "":
		cfg.Endpoint = os.Getenv(EnvEndpoint)
		if cfg.Region == "" {
			// R2's own answer, and the only region name it accepts. Defaulting
			// it here keeps the common invocation to two variables without
			// hiding the requirement: the tier still refuses an endpoint with
			// no region.
			cfg.Region = "auto"
		}
		if v := os.Getenv(EnvPathStyle); v != "" {
			b, perr := strconv.ParseBool(v)
			if perr != nil {
				return config.Store{}, false, fmt.Errorf("%s=%q is not a boolean", EnvPathStyle, v)
			}
			cfg.PathStyle = b
		}
	case truthy(os.Getenv(EnvAWS)):
		if cfg.Region == "" {
			return config.Store{}, false, fmt.Errorf("%s=1 needs %s", EnvAWS, EnvRegion)
		}
	default:
		return config.Store{}, false, fmt.Errorf(
			"%s is set but neither %s nor %s=1 is: refusing to guess which store that means", EnvBucket, EnvEndpoint, EnvAWS)
	}
	return cfg, true, nil
}

func truthy(v string) bool {
	b, err := strconv.ParseBool(v)
	return err == nil && b
}
