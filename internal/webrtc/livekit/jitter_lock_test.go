package livekit

import (
	"testing"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/server-sdk-go/v2/pkg/jitter"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
)

const jitterLatency = 200 * time.Millisecond

// vp8StartPacket is a one-packet VP8 frame. A buffer that got sequence
// number n drops any later packet with a lower one.
func vp8StartPacket(sn uint16) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{SequenceNumber: sn, Timestamp: uint32(sn) * 3000, Marker: true},
		Payload: []byte{0x10, 0x01},
	}
}

func closed(ch <-chan struct{}) func() bool {
	return func() bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
}

func adapterLockFree(lk *LiveKitWebRTC) func() bool {
	return func() bool {
		if !lk.m.TryLock() {
			return false
		}

		lk.m.Unlock()

		return true
	}
}

func TestJitterDropHandlerDoesNotTakeAdapterLock(t *testing.T) {
	lk, _ := setupMockLK()
	defer lk.Close()

	buffer := lk.addJitterBuffer("test-track", &codecs.VP8Packet{}, 90000, jitterLatency, 1234, true)
	buffer.Push(vp8StartPacket(1000))

	lk.m.Lock()

	pushed := make(chan struct{})

	go func() {
		defer close(pushed)
		buffer.Push(vp8StartPacket(990))
	}()

	assert.Eventually(t, closed(pushed), time.Second, time.Millisecond,
		"a dropped packet blocked on the adapter lock")
	lk.m.Unlock()
	<-pushed

	assert.Equal(t, uint64(1), buffer.Stats().PacketsDropped)
}

func TestGetStatsReadsJitterStatsWithoutAdapterLock(t *testing.T) {
	lk, _ := setupMockLK()
	defer lk.Close()

	// A drop handler that blocks keeps the buffer's lock held.
	inHandler := make(chan struct{})
	release := make(chan struct{})
	buffer := jitter.NewBuffer(&codecs.VP8Packet{}, 90000, jitterLatency,
		jitter.WithPacketDroppedHandler(func() {
			close(inHandler)
			<-release
		}))
	buffer.Push(vp8StartPacket(1000))

	lk.m.Lock()
	lk.jitterBuffers["test-track"] = buffer
	lk.remoteTrackPubs["test-track"] = &lksdk.RemoteTrackPublication{}
	lk.m.Unlock()

	go buffer.Push(vp8StartPacket(990))

	if !assert.Eventually(t, closed(inHandler), time.Second, time.Millisecond,
		"the buffer did not drop the late packet") {
		return
	}

	statsDone := make(chan struct{})

	go func() {
		defer close(statsDone)
		lk.GetStats()
	}()

	// GetStats copies the adapter maps in microseconds; give it ample time
	// to reach the buffer's lock before checking the adapter lock.
	time.Sleep(50 * time.Millisecond)

	assert.Eventually(t, adapterLockFree(lk), time.Second, time.Millisecond,
		"GetStats held the adapter lock while waiting for the jitter buffer")
	assert.False(t, closed(statsDone)(), "GetStats did not wait for the jitter buffer's lock")

	close(release)
	<-statsDone
}
