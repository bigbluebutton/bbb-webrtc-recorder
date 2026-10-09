package diskpressure

import "golang.org/x/sys/unix"

// blockSize returns the unit f_blocks and f_bavail are counted in.
func blockSize(fsStat *unix.Statfs_t) uint64 {
	// Some network and FUSE filesystems set f_frsize apart from f_bsize.
	if fsStat.Frsize > 0 {
		return uint64(fsStat.Frsize)
	}

	return uint64(fsStat.Bsize) //nolint:gosec // block size is never negative
}
