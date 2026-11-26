//go:build e2e && linux

package e2e

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus/oracle"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStartResponseNamesTheCodec starts one session for each of a VP8
// camera, an H.264 camera and a microphone. The cameras ask for a .webm
// name, as bbb-webrtc-sfu does.
//
// A requester must write the response's file name into the recording's
// events, or processing looks for a .webm that does not exist. So that name
// must have the extension of the file written. The response must also name
// the codec, and getRecordingsResponse must agree.
func TestStartResponseNamesTheCodec(t *testing.T) {
	const recordFor = 3 * time.Second

	s := NewStack(t, StackOptions{})
	room := "e2e-codecs-" + uuid.NewString()[:8]
	pub := s.NewPublisher(room, "e2e-publisher")

	vp8, err := corpus.GenerateVP8(corpus.VP8Options{FPS: 15, Duration: 30 * time.Second})
	require.NoError(t, err)
	opus, err := corpus.GenerateOpus(corpus.OpusOptions{Duration: 30 * time.Second})
	require.NoError(t, err)

	tracks := []struct {
		name   string
		track  *PubTrack
		asked  string // the extension of the requested name
		codecs []string
		ext    string
		codec  string
	}{
		{"vp8", pub.PublishVP8(TrackOptions{Name: "vp8", Schedule: vp8}), ".webm", []string{"video/vp8"}, ".webm", oracle.CodecVP8},
		{"h264", pub.PublishH264(TrackOptions{Name: "h264", Schedule: h264Camera(t, 15, 30*time.Second)}), ".webm", []string{mimeH264}, ".mkv", oracle.CodecH264},
		{"opus", pub.PublishOpus(TrackOptions{Name: "opus", Schedule: opus}), "", []string{"audio/opus"}, ".webm", oracle.CodecOpus},
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	recs := make([]*Recording, len(tracks))
	for i, tr := range tracks {
		require.NoError(t, pub.WaitPublished(ctx, tr.track))
		rec, err := s.Driver.StartRecordingWith(ctx, StartSpec{
			Room: room, TrackIDs: []string{tr.track.SID}, FileName: "recordings/" + room + "/" + tr.name + tr.asked,
		})
		require.NoError(t, err, tr.name)
		_, err = s.Driver.WaitRtpStatus(ctx, rec.From, rec.SessionID, "flowing")
		require.NoError(t, err, tr.name)
		recs[i] = rec
	}

	listed, err := s.Driver.GetRecordings(ctx)
	require.NoError(t, err)

	byID := map[string]*events.RecordingInfo{}
	for _, r := range listed {
		byID[r.SessionId] = r
	}

	time.Sleep(recordFor)

	for i, tr := range tracks {
		_, err := s.Driver.StopAndWait(ctx, recs[i].SessionID)
		require.NoError(t, err, tr.name)
	}

	for i, tr := range tracks {
		rec := recs[i]
		t.Logf("%s: %s %v", tr.name, rec.Path, rec.Codecs)

		assert.Equal(t, tr.codecs, rec.Codecs, "%s: startRecordingResponse codecs", tr.name)
		assert.Equal(t, tr.ext, filepath.Ext(rec.Path), "%s: startRecordingResponse fileName", tr.name)
		assert.Equal(t, filepath.Join(s.Recorder.Dir, strings.TrimSuffix(rec.FileName, tr.asked))+tr.ext, rec.Path, "%s: the requested name", tr.name)

		if info := byID[rec.SessionID]; assert.NotNil(t, info, "%s: getRecordingsResponse", tr.name) {
			assert.Equal(t, rec.Path, info.FileName, "%s: getRecordingsResponse fileName", tr.name)
			assert.Equal(t, tr.codecs, info.Codecs, "%s: getRecordingsResponse codecs", tr.name)
		}

		sum, err := oracle.SummarizeWebM(rec.Path)
		if assert.NoError(t, err, "%s: the file the response names", tr.name) {
			assert.NotNil(t, sum.Track(tr.codec), "%s: a %s track", tr.name, tr.codec)
		}
	}
}
