//go:build e2e && linux

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestH264CodecRegressionStopsAsClosed records an H.264 camera that lists
// VP8 as its backup, until a subscriber that takes VP8 only joins. LiveKit
// then ends the recorder's subscription instead of changing its codec in
// place.
//
// The session must stop by itself as closed, with an H.264 file and one
// payload type read. A new recording of the track, as bbb-webrtc-sfu starts
// after closed, must record VP8.
func TestH264CodecRegressionStopsAsClosed(t *testing.T) {
	const (
		fps = 15
		// LiveKit regresses the camera within a second of the subscriber
		// joining.
		stopWithin = 20 * time.Second
		recordFor  = 3 * time.Second
	)

	s := NewStack(t, StackOptions{})
	room := "e2e-h264-regression-" + uuid.NewString()[:8]
	pub := s.NewPublisher(room, "e2e-publisher")

	vp8, err := corpus.GenerateVP8(corpus.VP8Options{FPS: fps, Duration: 30 * time.Second, Width: 640, Height: 360})
	require.NoError(t, err)

	o := TrackOptions{Name: "camera", Source: livekit.TrackSource_CAMERA, Width: 640, Height: 360}
	primO, backO := o, o
	primO.Schedule = h264Camera(t, fps, 30*time.Second)
	backO.Schedule = vp8
	prim := newPubTrack(t, h264Capability, corpus.KindVideo, primO)
	back := newPubTrack(t, vp8Capability, corpus.KindVideo, backO)

	// The default backup codec policy, PREFER_REGRESSION, as livekit-client
	// publishes.
	lpub, err := pub.Room.LocalParticipant.PublishTrack(prim.Local, &lksdk.TrackPublicationOptions{
		Name: "camera", Source: livekit.TrackSource_CAMERA, VideoWidth: 640, VideoHeight: 360,
	}, lksdk.WithBackupCodec(back.Local))
	require.NoError(t, err)

	now := time.Now()
	for _, tr := range []*PubTrack{prim, back} {
		tr.Pub, tr.SID = lpub, lpub.SID()
		tr.startSending(now)
	}
	t.Cleanup(func() {
		prim.Stop()
		back.Stop()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	require.NoError(t, pub.WaitPublished(ctx, prim))

	rec := record(ctx, t, s, room, prim.SID)
	time.Sleep(recordFor)

	sub, err := lksdk.ConnectToRoomWithToken(s.LiveKit.URL, s.LiveKit.Token(t, room, "e2e-vp8-only", nil),
		&lksdk.RoomCallback{ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(track *webrtc.TrackRemote, _ *lksdk.RemoteTrackPublication, _ *lksdk.RemoteParticipant) {
				go func() {
					for {
						if _, _, err := track.ReadRTP(); err != nil {
							return
						}
					}
				}()
			},
		}},
		lksdk.WithAutoSubscribe(true),
		lksdk.WithCodecs([]livekit.Codec{{Mime: webrtc.MimeTypeVP8}, {Mime: webrtc.MimeTypeOpus}}))
	require.NoError(t, err)
	t.Cleanup(sub.Disconnect)

	stopCtx, stopCancel := context.WithTimeout(ctx, stopWithin)
	stop, err := s.Driver.WaitStopped(stopCtx, rec.From, rec.SessionID)
	stopCancel()
	require.NoError(t, err, "the session stops by itself once LiveKit regresses the camera")
	assert.Equal(t, "closed", stop.Reason)

	out := ReadOutput(t, rec, prim.SID)
	types := map[uint8]bool{}
	for _, r := range out.Trace.Records {
		if r.T == corpus.RecordRTP && !r.Pad {
			types[r.PT] = true
		}
	}
	assert.Len(t, types, 1, "media payload types read")
	assertTraceEnded(t, "first recording", out)
	require.Equal(t, oracle.CodecH264, out.WebM.Codec)
	assert.NoError(t, out.WebM.CheckH264())
	assert.Positive(t, out.WebM.H264Keyframes, "H.264 key frames written")

	rec2 := record(ctx, t, s, room, prim.SID)
	time.Sleep(recordFor)
	unrecord(ctx, t, s, "second recording", rec2)

	out2 := ReadOutput(t, rec2, prim.SID)
	assert.Equal(t, oracle.CodecVP8, out2.WebM.Codec, "the second recording's codec")
	assert.Positive(t, out2.WebM.Blocks, "the second recording's blocks")
}
