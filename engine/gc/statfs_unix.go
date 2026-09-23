//go:build linux || darwin

package gc

import (
	"fmt"
	"syscall"
)

// fsCapacity is the filesystem's total size in bytes.
//
// It is Blocks, not Bavail: the budget is a share of the WHOLE volume, and
// the floor is what leaves room for everything else on it. Deriving the
// budget from free space instead would make it shrink as the cache fills,
// which is a cache that converges on evicting itself to nothing.
//
// syscall.Statfs is used rather than golang.org/x/sys/unix because this
// module does not depend on x/sys and a cache tier is not a reason to add
// one. The two fields wanted here are stable across linux and darwin; the
// only difference is Bsize's width, which the conversion covers.
func fsCapacity(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("gc: statfs %s: %w", dir, err)
	}
	return int64(st.Blocks) * int64(st.Bsize), nil //nolint:gosec,unconvert // widths differ between linux and darwin
}
