package diskpressure

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
)

func TestBlockSizePrefersFragmentSize(t *testing.T) {
	assert.Equal(t, uint64(1024), blockSize(&unix.Statfs_t{Bsize: 4096, Frsize: 1024}))
	assert.Equal(t, uint64(4096), blockSize(&unix.Statfs_t{Bsize: 4096}))
}
