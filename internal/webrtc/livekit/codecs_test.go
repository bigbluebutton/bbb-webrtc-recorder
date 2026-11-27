package livekit

import (
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/prometheus/client_golang/prometheus"
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

func TestAcceptCodecTakesAudioAsItIs(t *testing.T) {
	rec := recorder.NewWebmRecorder(filepath.Join(t.TempDir(), "mic.webm"), 0o600, 256, 32, true, false)
	rec.SetHasAudio(true)
	h := newLoopHarness(rec, "mic")

	assert.True(t, h.w.acceptCodec("mic", TrackKindAudio, recorder.CodecOpus))
	assert.Equal(t, []string{"audio/opus"}, rec.Codecs())
}

func TestRecordingCodecsPickTheFormatFromTheBackupsToo(t *testing.T) {
	for _, c := range []struct {
		name   string
		info   *livekit.TrackInfo
		format recorder.Format
		codecs []string
	}{
		{
			name: "VP8 with an H.264 backup",
			info: &livekit.TrackInfo{MimeType: "video/VP8", Codecs: []*livekit.SimulcastCodecInfo{
				{MimeType: "video/VP8"}, {MimeType: "video/H264"},
			}},
			format: recorder.FormatMKV, codecs: []string{"video/vp8"},
		},
		{
			name: "H.264 with a VP8 backup",
			info: &livekit.TrackInfo{MimeType: "video/H264", Codecs: []*livekit.SimulcastCodecInfo{
				{MimeType: "video/H264"}, {MimeType: "video/VP8"},
			}},
			format: recorder.FormatMKV, codecs: []string{"video/h264"},
		},
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

func TestGetStatsNamesTheCodecWritten(t *testing.T) {
	h := newLoopHarness(newPushRecorder(), audioInfo.id)
	h.w.m.Lock()
	h.w.remoteTrackPubs[audioInfo.id] = &lksdk.RemoteTrackPublication{}
	h.w.trackStats[audioInfo.id] = &appstats.AdapterTrackStats{}
	h.w.trackCodecs = map[string]*recorder.Codec{audioInfo.id: recorder.CodecOpus}
	h.w.m.Unlock()

	tracks := h.w.GetStats().Tracks
	require.Len(t, tracks, 1)
	for _, st := range tracks {
		assert.Equal(t, "audio/opus", st.MimeType, "not the publication's MIME type")
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
	h, subscribed := subscribeHarness(t, rec, "TR_camera")

	_, err := h.w.subscribeToTracks([]string{"TR_camera"})
	require.NoError(t, err)

	assert.Equal(t, []string{"TR_camera"}, *subscribed)
	assert.Equal(t, recorder.FormatMKV, rec.Format(), "a format that holds H.264")
	assert.Equal(t, ".mkv", filepath.Ext(rec.GetFilePath()))
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

// A codec has one spelling in the stats file, LiveKit's, whether the
// publication or the subscribed codec names it.
func TestGetStatsSpellsTheCodecAsLiveKitDoes(t *testing.T) {
	camera := func(mimeType string) *livekit.TrackInfo {
		return &livekit.TrackInfo{Sid: "TR_1", Type: livekit.TrackType_VIDEO, Source: livekit.TrackSource_CAMERA, MimeType: mimeType}
	}
	for _, c := range []struct {
		name       string
		info       *livekit.TrackInfo
		subscribed *recorder.Codec
		mimeType   string
	}{
		{"VP8 camera", camera("video/VP8"), recorder.CodecVP8, "video/VP8"},
		{"H.264 camera", camera("video/H264"), recorder.CodecH264, "video/H264"},
		{"H.264 camera sent in its VP8 backup", camera("video/H264"), recorder.CodecVP8, "video/VP8"},
		{
			"RED microphone",
			&livekit.TrackInfo{Sid: "TR_1", Type: livekit.TrackType_AUDIO, Source: livekit.TrackSource_MICROPHONE, MimeType: "audio/red"},
			recorder.CodecOpus, "audio/opus",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newLoopHarness(newPushRecorder(), "TR_1")
			h.w.m.Lock()
			h.w.remoteTrackPubs["TR_1"] = remotePub(t, c.info)
			h.w.trackStats["TR_1"] = &appstats.AdapterTrackStats{}
			h.w.trackCodecs = map[string]*recorder.Codec{"TR_1": c.subscribed}
			h.w.m.Unlock()

			tracks := h.w.GetStats().Tracks
			require.Len(t, tracks, 1)
			for _, st := range tracks {
				assert.Equal(t, c.mimeType, st.MimeType)
				assert.Equal(t, c.info.MimeType, st.PubMimeType)
			}
		})
	}
}

// remotePub is a publication of info, made as lksdk makes one from a
// participant update.
func remotePub(t *testing.T, info *livekit.TrackInfo) *lksdk.RemoteTrackPublication {
	t.Helper()

	room := lksdk.NewRoom(nil)
	room.OnParticipantUpdate([]*livekit.ParticipantInfo{{Sid: "PA_publisher", Identity: "publisher", Tracks: []*livekit.TrackInfo{info}}})
	rp := room.GetParticipantByIdentity("publisher")
	require.NotNil(t, rp)
	pub, ok := rp.GetTrackPublication(info.GetSource()).(*lksdk.RemoteTrackPublication)
	require.True(t, ok)

	return pub
}

// The stats file names the codec written, but every capture metric keeps
// the publication's MIME type as its mime label.
func TestCaptureMetricsLabelARedMicrophoneAudioRed(t *testing.T) {
	h := newLoopHarness(newPushRecorder(), audioInfo.id)
	buffer := h.newBuffer(audioInfo, nil)
	defer buffer.close()
	buffer.push(opusPacket(1))
	h.w.addMediaClock(audioInfo, 48000)
	h.w.m.Lock()
	h.w.remoteTrackPubs[audioInfo.id] = remotePub(t, &livekit.TrackInfo{
		Sid: audioInfo.id, Type: livekit.TrackType_AUDIO, Source: livekit.TrackSource_MICROPHONE, MimeType: "audio/red",
	})
	h.w.trackCodecs = map[string]*recorder.Codec{audioInfo.id: recorder.CodecOpus}
	h.w.m.Unlock()

	stats := h.w.GetStats()
	require.Contains(t, stats.Tracks, "MICROPHONE")
	assert.Equal(t, "audio/opus", stats.Tracks["MICROPHONE"].MimeType)

	registry := prometheus.NewRegistry()
	registry.MustRegister(appstats.PLIRequests, appstats.PacketLossBuffer, appstats.SampleDuration,
		appstats.FrameSize, appstats.RTPDiscontinuities, appstats.VP8PictureIDDiscontinuities,
		appstats.KeyframeCount, appstats.CorruptedFrames, appstats.WrittenSamples,
		appstats.MediaClockInsertedGaps, appstats.MediaClockInsertedGapSeconds,
		appstats.MediaClockRateRatio, appstats.MediaClockEndLag)
	appstats.UpdateCaptureMetrics(stats)
	families, err := registry.Gather()
	require.NoError(t, err)

	mimes := map[string][]string{}
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, l := range metric.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["source"] == "MICROPHONE" {
				mimes[labels["mime"]] = append(mimes[labels["mime"]], family.GetName())
			}
		}
	}
	assert.Equal(t, []string{"audio/red"}, slices.Collect(maps.Keys(mimes)))
	assert.Len(t, mimes["audio/red"], 12, "every series but the media clock's rate and lag")
}
