package webrtc

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/utils"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

// panickingRecorder panics on the first video packet it is pushed.
type panickingRecorder struct{ recorder.Recorder }

func (panickingRecorder) PushVideo(*rtp.Packet)                           { panic("push failed") }
func (panickingRecorder) SetHasVideo(bool)                                {}
func (panickingRecorder) SetHasAudio(bool)                                {}
func (panickingRecorder) SetKeyframeRequester(recorder.KeyframeRequester) {}
func (panickingRecorder) VideoTimestamp() time.Duration                   { return 0 }
func (panickingRecorder) AudioTimestamp() time.Duration                   { return 0 }
func (panickingRecorder) Close() time.Duration                            { return 0 }

func TestTrackPanicFailsSession(t *testing.T) {
	ctx := context.WithValue(context.Background(), "session", "test-session") //nolint:staticcheck // SA1029: the key webrtc.go reads
	w := NewWebRTC(ctx, config.WebRTC{JitterBuffer: 512, JitterBufferPktTimeout: 200}, panickingRecorder{})

	states := make(chan utils.ConnectionState, 16)
	w.SetConnectionStateCallback(func(s utils.ConnectionState) { states <- s })
	w.SetFlowCallback(func(bool, time.Duration, bool) {})

	pub, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	defer func() { _ = pub.Close() }()

	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, "video", "stream")
	require.NoError(t, err)
	_, err = pub.AddTrack(track)
	require.NoError(t, err)

	offer, err := pub.CreateOffer(nil)
	require.NoError(t, err)
	gathered := webrtc.GatheringCompletePromise(pub)
	require.NoError(t, pub.SetLocalDescription(offer))
	<-gathered
	w.SetSDPOffer(*pub.LocalDescription())

	answer, err := w.Init()
	require.NoError(t, err)
	require.NoError(t, pub.SetRemoteDescription(answer))

	defer func() {
		w.SetConnectionStateCallback(func(utils.ConnectionState) {})
		w.Close()
	}()

	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	timeout := time.After(10 * time.Second)

	for seq := uint16(1); ; seq++ {
		select {
		case s := <-states:
			if s == utils.ConnectionStateFailed {
				return
			}
		case <-tick.C:
			_ = track.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: 3000 * uint32(seq), Marker: true},
				Payload: []byte{0x10, 0x00, 0x9d, 0x01, 0x2a},
			})
		case <-timeout:
			require.FailNow(t, "the session never reported failed")
		}
	}
}

// The PLI ticker, the recorder's keyframe requests and a second video track's
// arrival run on different goroutines.
func TestConcurrentKeyframeRequests(t *testing.T) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	defer func() { _ = pc.Close() }()

	// Every PLI on the unconnected PeerConnection logs an error.
	level := log.GetLevel()
	log.SetLevel(log.FatalLevel)
	t.Cleanup(func() { log.SetLevel(level) })

	w := NewWebRTC(context.Background(), config.WebRTC{}, nil)
	w.pc = pc
	w.addVideoTrack(1111)

	var wg sync.WaitGroup
	for _, run := range []func(){
		func() {
			for range 50000 {
				w.RequestKeyframeForSSRC(1111)
			}
		},
		func() {
			for range 200 {
				w.RequestKeyframe()
			}
		},
		func() {
			for i := range 2000 {
				w.addVideoTrack(uint32(2000 + i))
			}
		},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run()
		}()
	}
	wg.Wait()
}
