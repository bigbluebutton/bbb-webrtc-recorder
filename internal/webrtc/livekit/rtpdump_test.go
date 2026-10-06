package livekit

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/rtpdump"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readRTPDump is the RTP packets of the rtpdump file at path.
func readRTPDump(t *testing.T, path string) []*rtp.Packet {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	r, _, err := rtpdump.NewReader(f)
	require.NoError(t, err)

	var packets []*rtp.Packet
	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			return packets
		}
		require.NoError(t, err)
		require.False(t, p.IsRTCP)

		pkt := &rtp.Packet{}
		require.NoError(t, pkt.Unmarshal(p.Payload))
		packets = append(packets, pkt)
	}
}

func dumpAudioPackets() []*rtp.Packet {
	packets := make([]*rtp.Packet, 0, 20)
	for i := range 20 {
		p := opusPacket(100 + uint16(i))
		p.SSRC = audioInfo.ssrc
		packets = append(packets, p)
	}

	return packets
}

func dumpVideoPackets() []*rtp.Packet {
	packets := make([]*rtp.Packet, 0, 10)
	for i := range 10 {
		p := vp8Packet(500+uint16(i), 90000+uint32(i)*3000, true, true)
		p.SSRC = videoInfo.ssrc
		packets = append(packets, p)
	}

	return packets
}

func TestRTPDumpOneFilePerTrack(t *testing.T) {
	dir := t.TempDir()

	synctest.Test(t, func(t *testing.T) {
		rec := newPushRecorder()
		rec.filePath = filepath.Join(dir, "rec.webm")
		h := newLoopHarness(rec, audioInfo.id, videoInfo.id)

		audioReads := make([]trackRead, 0, 20)
		for i, p := range dumpAudioPackets() {
			audioReads = append(audioReads, readsAt(time.Duration(i)*20*time.Millisecond, p)...)
		}

		videoReads := make([]trackRead, 0, 10)
		for i, p := range dumpVideoPackets() {
			videoReads = append(videoReads, readsAt(time.Duration(i)*33*time.Millisecond, p)...)
		}

		// Both subscriptions, dump included, run at once as pion's OnTrack
		// goroutines do.
		var wg sync.WaitGroup
		for _, track := range []struct {
			info  trackInfo
			reads []trackRead
		}{{audioInfo, audioReads}, {videoInfo, videoReads}} {
			wg.Go(func() {
				h.w.openRTPDump(track.info.id)
				h.run(newFakeTrack(track.reads), track.info)
			})
		}
		wg.Wait()

		h.w.Close()
	})

	assert.NoFileExists(t, filepath.Join(dir, "rec.rtp"))

	for _, tc := range []struct {
		id   string
		want []*rtp.Packet
	}{{audioInfo.id, dumpAudioPackets()}, {videoInfo.id, dumpVideoPackets()}} {
		got := readRTPDump(t, filepath.Join(dir, "rec."+tc.id+".rtp"))
		require.Len(t, got, len(tc.want), tc.id)
		for i := range tc.want {
			assert.Equal(t, tc.want[i].SSRC, got[i].SSRC, tc.id)
			assert.Equal(t, tc.want[i].SequenceNumber, got[i].SequenceNumber, tc.id)
			assert.Equal(t, tc.want[i].Timestamp, got[i].Timestamp, tc.id)
			assert.Equal(t, tc.want[i].Marker, got[i].Marker, tc.id)
			assert.Equal(t, tc.want[i].Payload, got[i].Payload, tc.id)
		}
	}
}

// A track subscribed again starts its dump over, which the earlier writer,
// still held by the earlier read loop, does not corrupt.
func TestRTPDumpReopen(t *testing.T) {
	dir := t.TempDir()
	rec := newPushRecorder()
	rec.filePath = filepath.Join(dir, "rec.webm")
	h := newLoopHarness(rec, audioInfo.id)

	h.w.openRTPDump(audioInfo.id)
	first := h.w.rtpWriters[audioInfo.id]
	for _, p := range dumpAudioPackets() {
		require.NoError(t, first.WriteRTP(p))
	}

	h.w.openRTPDump(audioInfo.id)
	require.NoError(t, first.WriteRTP(opusPacket(999)))
	second := []*rtp.Packet{opusPacket(500), opusPacket(501)}
	for _, p := range second {
		require.NoError(t, h.w.rtpWriters[audioInfo.id].WriteRTP(p))
	}
	h.w.Close()

	got := readRTPDump(t, filepath.Join(dir, "rec."+audioInfo.id+".rtp"))
	require.Len(t, got, len(second))
	for i := range second {
		assert.Equal(t, second[i].SequenceNumber, got[i].SequenceNumber)
	}
}

func TestTrackFilePath(t *testing.T) {
	assert.Equal(t, "/rec/a.TR_x.rtp", trackFilePath("/rec/a.webm", "TR_x", ".rtp"))
	assert.Equal(t, "/rec/a.room_cam_1-b.rtp", trackFilePath("/rec/a.webm", "room/cam 1-b", ".rtp"))
	assert.Equal(t, "/rec/a.___.rtp", trackFilePath("/rec/a", "../", ".rtp"))
}
