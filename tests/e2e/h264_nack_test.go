//go:build e2e && linux

package e2e

import (
	"testing"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestH264NACKRepairUnderLoss is TestNACKRepairUnderLoss on an H.264 camera.
// The file must also pass the oracle's H.264 checks.
func TestH264NACKRepairUnderLoss(t *testing.T) {
	res := checkNACKRepairUnderLoss(t, corpus.CodecH264)

	require.Equal(t, oracle.CodecH264, res.out.WebM.Codec)
	assert.NoError(t, res.out.WebM.CheckH264())
}
