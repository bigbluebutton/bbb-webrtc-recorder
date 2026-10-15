package livekit

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
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

				synctest.Wait() // flow events are delivered asynchronously
				require.False(t, firstFlowing.IsZero(), "no flowing event")
				assert.False(t, firstFlowing.Before(start), "flowing at %v, before the first frame", firstFlowing.Sub(start))
				assert.False(t, firstFlowing.After(start.Add(frameInterval)),
					"flowing at %v, after the second frame", firstFlowing.Sub(start))
				assert.Equal(t, []bool{true}, flows, "a resent packet is no stall")
			})
		})
	}
}

// flowRecorder records the flow events an adapter delivers. The first one
// blocks until release is closed, as a PUBLISH to a stalled Redis does.
type flowRecorder struct {
	mu         sync.Mutex
	events     []string
	inFlight   int
	overlapped bool // a delivery started while another was still running
	entered    chan struct{}
	release    chan struct{}
}

func newFlowAdapter(t *testing.T) (*LiveKitWebRTC, *flowRecorder) {
	t.Helper()
	w := &LiveKitWebRTC{
		ctx:       context.WithValue(context.Background(), "session", "test-session"), //nolint:staticcheck // SA1029: the key livekit.go reads
		flowState: map[string]*trackFlowState{},
		startTs:   time.Now(),
	}
	r := &flowRecorder{entered: make(chan struct{}, 1), release: make(chan struct{})}
	first := true
	w.flowCallback = func(isFlowing bool, _ time.Duration, closed bool) {
		r.mu.Lock()
		r.events = append(r.events, fmt.Sprintf("flowing=%v closed=%v", isFlowing, closed))
		r.inFlight++
		r.overlapped = r.overlapped || r.inFlight > 1
		block := first
		first = false
		r.mu.Unlock()
		if block {
			r.entered <- struct{}{}
			<-r.release
		}
		r.mu.Lock()
		r.inFlight--
		r.mu.Unlock()
	}
	t.Cleanup(w.stopFlowEvents)

	return w, r
}

// delivered waits until n events have been delivered and returns them.
func (r *flowRecorder) delivered(t *testing.T, n int) []string {
	t.Helper()
	var events []string
	require.Eventually(t, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		events = append([]string(nil), r.events...)

		return len(events) >= n
	}, time.Second, time.Millisecond)

	return events
}

// within reports whether fn returns within a second; fn keeps running if not.
func within(fn func()) bool {
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(time.Second):
		return false
	}
}

// A flow event's delivery reaches Redis. It must not hold the adapter lock
// that every track's packet intake takes.
func TestFlowEventDeliveryDoesNotHoldTheAdapterLock(t *testing.T) {
	w, r := newFlowAdapter(t)
	defer close(r.release)
	now := time.Now()
	w.updateFlowState("video", 1, now)
	w.updateFlowState("audio", 1, now)
	go w.updateFlowState("video", 2, now) // video starts flowing
	select {
	case <-r.entered:
	case <-time.After(time.Second):
		require.FailNow(t, "no flow event delivered")
	}

	assert.True(t, within(func() { w.updateFlowState("audio", 1, now) }),
		"audio's flow update waited for video's flow event to be delivered")
}

// The session must see transitions in the order they happened, the
// disconnect's closed event included.
func TestFlowEventsArriveInTransitionOrder(t *testing.T) {
	w, r := newFlowAdapter(t)
	now := time.Now()
	w.updateFlowState("video", 1, now)
	assert.True(t, within(func() {
		w.updateFlowState("video", 2, now) // flowing; its delivery blocks
		<-r.entered
		w.updateFlowState("video", 2, now) // stalled
		w.updateFlowState("video", 3, now) // flowing again
		w.onDisconnected(lksdk.LeaveRequested)
	}), "transitions waited for a delivery")
	close(r.release)

	assert.Equal(t, []string{
		"flowing=true closed=false",
		"flowing=false closed=false",
		"flowing=true closed=false",
		"flowing=false closed=true",
	}, r.delivered(t, 4))
	r.mu.Lock()
	defer r.mu.Unlock()
	assert.False(t, r.overlapped, "a flow event was delivered while another was still being delivered")
}

// A session stops by installing a no-op callback and closing the adapter.
// No flow event may reach it after that, so the stop waits for an event
// being delivered.
func TestStopWaitsForTheFlowEventBeingDelivered(t *testing.T) {
	w, r := newFlowAdapter(t)
	now := time.Now()
	w.updateFlowState("video", 1, now)
	go w.updateFlowState("video", 2, now) // flowing; its delivery blocks
	<-r.entered

	stopped := make(chan struct{})
	go func() {
		w.stopFlowEvents()
		close(stopped)
	}()
	select {
	case <-stopped:
		assert.Fail(t, "the stop returned while a flow event was being delivered")
	case <-time.After(100 * time.Millisecond):
	}

	close(r.release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		require.FailNow(t, "the stop did not return once the delivery ended")
	}

	w.updateFlowState("video", 2, now) // stalled, after the stop
	r.mu.Lock()
	defer r.mu.Unlock()
	assert.Equal(t, []string{"flowing=true closed=false"}, r.events, "no flow event after the stop")
}
