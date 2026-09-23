//go:build !linux && !darwin

package gc

// fsCapacity has no statfs to ask on this platform.
//
// It reports a budget large enough never to bind, so that the package builds
// and its tests run anywhere while the real limit is whatever the filesystem
// enforces. That is the right trade for a platform the server is never
// deployed on: `go test` on a developer's machine has to work, and a wrong
// budget there would only make the tests lie.
func fsCapacity(string) (int64, error) { return int64(1) << 60, nil }
