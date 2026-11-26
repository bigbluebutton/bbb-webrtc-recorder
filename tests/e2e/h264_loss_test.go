//go:build e2e && linux

package e2e

import (
	"fmt"
	"testing"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
)

// TestH264UnrepairedLossRequestsKeyframe is TestUnrepairedLossRequestsKeyframe
// on an H.264 camera. Each frame is one slice sent as FU-A fragments, and a
// middle fragment is withheld. The file must skip that frame and each one
// after it, up to the key frame the recorder's request brings.
func TestH264UnrepairedLossRequestsKeyframe(t *testing.T) {
	for _, sc := range unrepairedLossScenarios(corpus.CodecH264) {
		t.Run(fmt.Sprintf("%gfps", sc.fps), func(t *testing.T) { checkUnrepairedLoss(t, sc) })
	}
}
