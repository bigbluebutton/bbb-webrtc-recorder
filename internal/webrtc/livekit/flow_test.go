package livekit

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The SFU stamps a recording's start when the first flowing event reaches it.
// A first key frame missing a packet must not hold that event back, and the
// padding LiveKit sends ahead of the first frame must not bring it forward.
func TestFirstFlowingFollowsArrival(t *testing.T) {
	const frameInterval = time.Second / 30

	for _, tc := range []struct {
		name string
		lost bool
		late time.Duration // when the lost packet comes after its frame, if set
	}{
		{name: "complete first frame"},
		{name: "first frame repaired late", lost: true, late: 600 * time.Millisecond},
		{name: "first frame never repaired", lost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w := &LiveKitWebRTC{
					ctx:       context.WithValue(context.Background(), "session", "test-session"), //nolint:staticcheck // SA1029: the key livekit.go reads
					flowState: map[string]*trackFlowState{},
				}
				var flows []bool
				var firstFlowing time.Time
				w.flowCallback = func(isFlowing bool, _ time.Duration, _ bool) {
					flows = append(flows, isFlowing)
					if isFlowing && firstFlowing.IsZero() {
						firstFlowing = time.Now()
					}
				}
				b := newTrackBuffer(&codecs.VP8Packet{}, videoJitterLatency, lossKeyframeInterval, func() {})
				defer b.close()
				push := w.packetPusher("video", b)

				seq := uint16(1000)
				for range 3 {
					// LiveKit's padding-on-mute as pion reads it; see packetPusher.
					push(&rtp.Packet{Header: rtp.Header{SequenceNumber: seq, Padding: true, PaddingSize: 255}, Payload: make([]byte, 255)})
					seq++
					time.Sleep(100 * time.Millisecond)
				}

				start := time.Now()
				var held *rtp.Packet
				for f := range 30 {
					time.Sleep(time.Until(start.Add(time.Duration(f) * frameInterval)))
					if held != nil && time.Since(start) >= tc.late {
						push(held)
						held = nil
					}

					for j := range 3 {
						p := vp8Packet(seq, uint32(3000*(f+1)), j == 0, j == 2)
						seq++
						if tc.lost && f == 0 && j == 1 {
							if tc.late > 0 {
								held = p
							}

							continue
						}
						push(p)
						if f == 10 && j == 2 {
							push(p) // as an RTX probe resends the newest packet
						}
					}
				}

				require.False(t, firstFlowing.IsZero(), "no flowing event")
				assert.False(t, firstFlowing.Before(start), "flowing at %v, before the first frame", firstFlowing.Sub(start))
				assert.False(t, firstFlowing.After(start.Add(frameInterval)),
					"flowing at %v, after the second frame", firstFlowing.Sub(start))
				assert.Equal(t, []bool{true}, flows, "a resent packet is no stall")
			})
		})
	}
}
