package livekit

import (
	"flag"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var updateFence = flag.Bool("update", false, "pin in testdata/corpus-fence.json the values this run measures")

const corpusFencePath = "testdata/corpus-fence.json"

// corpusFenceEntry is what the fence pins for a seed: the recording's
// bytes, every keyframe request and the packet counts.
type corpusFenceEntry struct {
	File             string  `json:"file"`
	BufferRequestsUs []int64 `json:"bufferRequestsUs"`
	WriterRequestsUs []int64 `json:"writerRequestsUs"`
	Packets          int     `json:"packets"`
	PacketsLost      uint64  `json:"packetsLost"`
	PacketsDropped   uint64  `json:"packetsDropped"`
}

func microseconds(ds []time.Duration) []int64 {
	us := make([]int64, 0, len(ds))
	for _, d := range ds {
		us = append(us, d.Microseconds())
	}

	return us
}

func corpusFenceEntryOf(r replayResult) corpusFenceEntry {
	return corpusFenceEntry{
		File:             oracle.SHA256(r.file),
		BufferRequestsUs: microseconds(r.bufferRequests),
		WriterRequestsUs: microseconds(r.writerRequests),
		Packets:          r.packets,
		PacketsLost:      r.packetsLost,
		PacketsDropped:   r.packetsDropped,
	}
}

func newCorpusFence(t *testing.T) *oracle.Fence {
	t.Helper()

	f, err := oracle.NewFence(corpusFencePath, *updateFence)
	require.NoError(t, err)

	t.Cleanup(func() {
		unmeasured, err := f.Close()
		require.NoError(t, err)
		// A -run or -skip filter replays only some seeds.
		if flag.Lookup("test.run").Value.String() == "" && flag.Lookup("test.skip").Value.String() == "" {
			assert.Empty(t, unmeasured, "seeds pinned in %s that no replay measured; -update keeps the pin of a removed seed, so delete it by hand", corpusFencePath)
		}
	})

	return f
}

func checkCorpusFence(t *testing.T, f *oracle.Fence, name string, r replayResult) {
	t.Helper()

	diff, err := f.Check(name, corpusFenceEntryOf(r))
	require.NoError(t, err)
	assert.Empty(t, diff, "the recording or its keyframe requests changed; if intended, run with -update")
}
