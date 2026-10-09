//go:build !linux

package diskpressure

import "golang.org/x/sys/unix"

// blockSize returns the unit f_blocks and f_bavail are counted in. To be used
// in builds other than Linux.
func blockSize(fsStat *unix.Statfs_t) uint64 {
	return uint64(fsStat.Bsize) //nolint:gosec // block size is never negative
}
