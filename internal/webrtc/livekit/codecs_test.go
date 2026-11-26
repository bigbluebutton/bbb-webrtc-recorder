package livekit

import (
	"path/filepath"
	"testing"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrackCodecsListsThePrimaryThenTheBackup(t *testing.T) {
	assert.Nil(t, trackCodecs(nil))
	assert.Equal(t, []string{"video/vp8"}, trackCodecs(&livekit.TrackInfo{MimeType: "video/VP8"}))
	assert.Equal(t, []string{"video/h264", "video/vp8"}, trackCodecs(&livekit.TrackInfo{
		MimeType: "video/H264",
		Codecs:   []*livekit.SimulcastCodecInfo{{MimeType: "video/H264"}, {MimeType: "video/VP8"}},
	}))
	assert.Equal(t, []string{"video/vp8"}, trackCodecs(&livekit.TrackInfo{
		Codecs: []*livekit.SimulcastCodecInfo{{MimeType: ""}, {MimeType: "video/VP8"}},
	}), "a primary with no MIME type is skipped")
}

func TestTrackCodecsNamesARedMicrophoneOpus(t *testing.T) {
	// BBB publishes its microphone with RED.
	info := &livekit.TrackInfo{MimeType: "audio/red", Codecs: []*livekit.SimulcastCodecInfo{{MimeType: "audio/red"}}}

	assert.Equal(t, []string{"audio/opus"}, trackCodecs(info))
	assert.Equal(t, recorder.FormatWebM, recorder.FormatFor(trackCodecs(info)))
}

func TestRecordingCodecsPickTheFormatFromTheBackupsToo(t *testing.T) {
	for _, c := range []struct {
		name   string
		info   *livekit.TrackInfo
		format recorder.Format
		codecs []string
	}{
		{
			name: "VP8 with an H.265 backup",
			info: &livekit.TrackInfo{MimeType: "video/VP8", Codecs: []*livekit.SimulcastCodecInfo{
				{MimeType: "video/VP8"}, {MimeType: "video/H265"},
			}},
			format: recorder.FormatWebM, codecs: []string{"video/vp8"},
		},
		{
			name:   "RED microphone",
			info:   &livekit.TrackInfo{MimeType: "audio/red", Codecs: []*livekit.SimulcastCodecInfo{{MimeType: "audio/red"}}},
			format: recorder.FormatWebM, codecs: []string{"audio/opus"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			format, codecs := recordingCodecs([]*livekit.TrackInfo{c.info})
			assert.Equal(t, c.format, format)
			assert.Equal(t, c.codecs, codecs)
		})
	}
}

// publisherRoom is a room with one participant, as lksdk lists it. It
// publishes an H.264 camera with a VP8 backup, a RED microphone and a
// screen share that lists no codec.
func publisherRoom() *lksdk.Room {
	room := lksdk.NewRoom(nil)
	room.OnParticipantUpdate([]*livekit.ParticipantInfo{{Sid: "PA_publisher", Identity: "publisher", Tracks: []*livekit.TrackInfo{
		{
			Sid: "TR_camera", Type: livekit.TrackType_VIDEO, Source: livekit.TrackSource_CAMERA, MimeType: "video/H264",
			Codecs: []*livekit.SimulcastCodecInfo{{MimeType: "video/H264"}, {MimeType: "video/VP8"}},
		},
		{Sid: "TR_mic", Type: livekit.TrackType_AUDIO, Source: livekit.TrackSource_MICROPHONE, MimeType: "audio/red"},
		{Sid: "TR_screen", Type: livekit.TrackType_VIDEO, Source: livekit.TrackSource_SCREEN_SHARE},
	}}})

	return room
}

// subscribeHarness is a loopHarness on publisherRoom whose subscriptions
// only note the track IDs.
func subscribeHarness(t *testing.T, rec recorder.Recorder, trackIDs ...string) (*loopHarness, *[]string) {
	t.Helper()

	h := newLoopHarness(rec, trackIDs...)
	h.w.room = publisherRoom()
	h.w.sendVideoQuality = func(string, *lksdk.RemoteTrackPublication, livekit.VideoQuality) {}
	var subscribed []string
	h.w.setSubscribed = func(pub *lksdk.RemoteTrackPublication) error {
		subscribed = append(subscribed, pub.SID())

		return nil
	}

	return h, &subscribed
}

// Init sets the recording's format, and keeps the codecs for the start
// response.
func TestSubscribeToTracksSetsTheFormatAndKeepsTheCodecs(t *testing.T) {
	rec := recorder.NewWebmRecorder(filepath.Join(t.TempDir(), "camera.webm"), 0o600, 256, 32, true, false)
	require.NoError(t, rec.SetFormat(recorder.FormatMKV))
	h, subscribed := subscribeHarness(t, rec, "TR_camera")

	_, err := h.w.subscribeToTracks([]string{"TR_camera"})
	require.NoError(t, err)

	assert.Equal(t, []string{"TR_camera"}, *subscribed)
	assert.Equal(t, recorder.FormatWebM, rec.Format(), "WebM holds every codec the recorder writes")
	assert.Equal(t, ".webm", filepath.Ext(rec.GetFilePath()))
	assert.Equal(t, []string{"video/h264"}, h.w.Codecs())
}

// The codecs follow the request's track IDs, though lksdk lists the
// publications in map order.
func TestSubscribeToTracksKeepsTheRequestOrder(t *testing.T) {
	for _, c := range []struct {
		trackIDs, subscribed, codecs []string
	}{
		{[]string{"TR_camera", "TR_mic"}, []string{"TR_camera", "TR_mic"}, []string{"video/h264", "audio/opus"}},
		{[]string{"TR_mic", "TR_camera"}, []string{"TR_mic", "TR_camera"}, []string{"audio/opus", "video/h264"}},
		{[]string{"TR_mic", "TR_missing", "TR_camera"}, []string{"TR_mic", "TR_camera"}, []string{"audio/opus", "video/h264"}},
		{[]string{"TR_screen", "TR_camera"}, []string{"TR_screen", "TR_camera"}, []string{"video/h264"}},
	} {
		for range 20 {
			h, subscribed := subscribeHarness(t, newPushRecorder(), c.trackIDs...)
			_, err := h.w.subscribeToTracks(c.trackIDs)
			require.NoError(t, err)

			require.Equal(t, c.subscribed, *subscribed)
			require.Equal(t, c.codecs, h.w.Codecs())
		}
	}
}
