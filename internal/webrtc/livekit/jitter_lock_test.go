package livekit

import (
	"testing"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
)

const jitterLatency = 200 * time.Millisecond

// vp8StartPacket is a one-packet VP8 frame.
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

// lostAfterGap pushes two frames around a missing sequence number. The
// buffer reports the loss from its timer, with its lock held, once the
// second frame's latency expires.
func lostAfterGap(b *trackBuffer) {
	b.push(vp8StartPacket(1000))
	b.push(vp8StartPacket(1002))
}

func TestJitterLossHandlerDoesNotTakeAdapterLock(t *testing.T) {
	lk, _ := setupMockLK()
	defer lk.Close()

	buffer := lk.addJitterBuffer("test-track", &codecs.VP8Packet{}, jitterLatency, 1234, true)
	defer buffer.close()

	lk.m.Lock()
	lostAfterGap(buffer)

	// stats takes the buffer's lock, which a handler waiting for the
	// adapter lock would keep held.
	lost := make(chan struct{})

	go func() {
		defer close(lost)

		for buffer.stats().PacketsLost == 0 {
			time.Sleep(time.Millisecond)
		}
	}()

	assert.Eventually(t, closed(lost), jitterLatency+time.Second, time.Millisecond,
		"a lost packet blocked on the adapter lock")
	lk.m.Unlock()
	<-lost
}

func TestGetStatsReadsJitterStatsWithoutAdapterLock(t *testing.T) {
	lk, _ := setupMockLK()
	defer lk.Close()

	// A loss handler that blocks keeps the buffer's lock held.
	inHandler := make(chan struct{})
	release := make(chan struct{})
	buffer := newTrackBuffer(&codecs.VP8Packet{}, jitterLatency, 0, func() {
		close(inHandler)
		<-release
	})
	defer buffer.close()

	lk.m.Lock()
	lk.jitterBuffers["test-track"] = buffer
	lk.remoteTrackPubs["test-track"] = &lksdk.RemoteTrackPublication{}
	lk.m.Unlock()

	lostAfterGap(buffer)

	if !assert.Eventually(t, closed(inHandler), jitterLatency+time.Second, time.Millisecond,
		"the buffer did not report the loss") {
		close(release)

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
