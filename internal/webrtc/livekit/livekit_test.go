package livekit

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/interfaces"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/stretchr/testify/assert"
)

// mockRecorder implements the recorder.Recorder interface for testing
type mockRecorder struct {
	videoStats *types.RecorderTrackStats
	audioStats *types.RecorderTrackStats
	hasAudio   bool
	hasVideo   bool
	filePath   string
}

func (m *mockRecorder) GetFilePath() string {
	return m.filePath
}

func (m *mockRecorder) GetStats() *types.RecorderStats {
	return &types.RecorderStats{
		Video: m.videoStats,
		Audio: m.audioStats,
	}
}

func (m *mockRecorder) PushVideo(packet *rtp.Packet)                                {}
func (m *mockRecorder) PushAudio(packet *rtp.Packet)                                {}
func (m *mockRecorder) NotifySkippedPacket(seq uint16)                              {}
func (m *mockRecorder) WithContext(ctx context.Context)                             {}
func (m *mockRecorder) VideoTimestamp() time.Duration                               { return 0 }
func (m *mockRecorder) AudioTimestamp() time.Duration                               { return 0 }
func (m *mockRecorder) SetHasAudio(hasAudio bool)                                   { m.hasAudio = hasAudio }
func (m *mockRecorder) SetHasVideo(hasVideo bool)                                   { m.hasVideo = hasVideo }
func (m *mockRecorder) SetKeyframeRequester(requester interfaces.KeyframeRequester) {}
func (m *mockRecorder) GetHasAudio() bool                                           { return m.hasAudio }
func (m *mockRecorder) GetHasVideo() bool                                           { return m.hasVideo }
func (m *mockRecorder) Close() time.Duration                                        { return 0 }

func (m *mockRecorder) PushVideoAt(packet *rtp.Packet, at time.Time) {}
func (m *mockRecorder) PushAudioAt(packet *rtp.Packet, at time.Time) {}
func (m *mockRecorder) MediaSpan() (types.MediaSpan, bool)           { return types.MediaSpan{}, false }

func TestProcessPacketStats_SequenceNumberWraparound(t *testing.T) {
	lk, _ := setupMockLK()
	trackIds := lk.trackIds
	packets := makeFullRangePackets(0)

	// Verify wraparound count 0 - we should be at 65530
	lk.processPacketStats(trackIds[0], packets[:65531])
	trackStats := lk.trackStats[trackIds[0]]
	assert.Equal(t, 0, trackStats.SeqNumWrapArounds,
		"Should detect no sequence number wraparounds")

	// Wrap around exactly to zero
	lk.processPacketStats(trackIds[0], append(packets[65530:], packets[:1]...))
	assert.Equal(t, 1, trackStats.SeqNumWrapArounds,
		"Should detect one sequence number wraparound (exactly to zero)")

	// Induce two more wraparounds by processsing the same packets again three times.
	// Do it in batches of 250 packets to test the wraparound detection logic.
	// We should end at 65534
	for i := 0; i < 3; i++ {
		clonedPackets := make([]*rtp.Packet, len(packets))
		copy(clonedPackets, packets)
		for j := 0; j < len(clonedPackets); j += 250 {
			end := j + 250

			if end > len(clonedPackets) {
				end = len(clonedPackets)
			}
			lk.processPacketStats(trackIds[0], clonedPackets[j:end])
		}
	}

	assert.Equal(t, 3, trackStats.SeqNumWrapArounds,
		"Should detect 3 sequence number wraparounds")
}

// Test wraparound detection with pre-initialized track stats
func TestProcessPacketStats_PreInitializedTrackStatsWraparound(t *testing.T) {
	lk, _ := setupMockLK()
	trackIds := lk.trackIds
	packets := makeFullRangePackets(0)
	lk.trackStats[trackIds[0]] = &appstats.AdapterTrackStats{
		StartTime:         time.Now().Unix(),
		EndTime:           time.Now().Unix(),
		FirstSeqNum:       0,
		LastSeqNum:        0,
		SeqNumWrapArounds: 0,
		PLIRequests:       0,
		RTPReadErrors:     0,
	}
	// Process all initial packets (no wraparounds)
	lk.processPacketStats(trackIds[0], packets)
	assert.Equal(t, 0, lk.trackStats[trackIds[0]].SeqNumWrapArounds,
		"Should detect no sequence number wraparound")

	// Process packets with wraparound - from 65530 to 0 to 65531
	lk.processPacketStats(trackIds[0], append(packets[65530:], packets[:65532]...))
	assert.Equal(t, 1, lk.trackStats[trackIds[0]].SeqNumWrapArounds,
		"Should detect one sequence number wraparound")
}

func TestDoubleClose(t *testing.T) {
	lk, _ := setupMockLK()

	// First close should succeed
	firstDuration := lk.Close()
	assert.Equal(t, time.Duration(0), firstDuration, "First close should return duration from recorder")

	time.Sleep(50 * time.Millisecond)

	// Second close should not panic
	secondDuration := lk.Close()
	assert.Equal(t, time.Duration(0), secondDuration, "Second close should still return same duration")
	assert.Nil(t, lk.room, "Room should be nil after close")
	assert.NotNil(t, lk.keyframeRequestChan, "Channel should not be nil even after close")
}

// GetStats can run while a track subscribes, which writes the maps it reads
// under w.m. Run with -race. pliStats is read only for video publications,
// which a test cannot build, so its copy goes unchecked.
func TestGetStatsDuringSubscription(t *testing.T) {
	h := newLoopHarness(newPushRecorder(), audioInfo.id)

	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 1000 {
			h.w.registerTrackBuffer("late", true, 4321, nil)

			trackID := fmt.Sprintf("late-%d", i)
			h.w.m.Lock()
			h.w.remoteTrackPubs[trackID] = &lksdk.RemoteTrackPublication{}
			h.w.trackStats[trackID] = &appstats.AdapterTrackStats{}
			h.w.m.Unlock()
		}
	})
	wg.Go(func() {
		for range 200 {
			assert.NotNil(t, h.w.GetStats())
		}
	})
	wg.Wait()
}

func setupMockLK() (*LiveKitWebRTC, *mockRecorder) {
	ctx := context.Background()
	ctx = context.WithValue(ctx, "session", "test-session")
	cfg := config.LiveKit{
		Host:      "test-host",
		APIKey:    "test-key",
		APISecret: "test-secret",
	}
	rec := &mockRecorder{
		videoStats: &types.RecorderTrackStats{},
		audioStats: &types.RecorderTrackStats{},
		filePath:   "test.webm",
	}
	roomID := "test-room"
	trackIDs := []string{"test-track"}
	lk := NewLiveKitWebRTC(ctx, cfg, rec, roomID, trackIDs)

	return lk, rec
}

func makeFullRangePackets(start uint16) []*rtp.Packet {
	packets := make([]*rtp.Packet, 65535)

	for i := 0; i < 65535; i++ {
		packets[i] = &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i * 40),
			},
		}
	}

	return packets
}

// lksdk runs mute callbacks for any track in the room, so they can race
// the subscribe loop registering this capture's tracks.
func TestTrackMuteCallbacksDuringTrackRegistration(t *testing.T) {
	lk, _ := setupMockLK()
	defer lk.Close()

	lk.sendVideoQuality = func(string, *lksdk.RemoteTrackPublication, livekit.VideoQuality) {}
	pub := &lksdk.RemoteTrackPublication{}
	done := make(chan struct{})

	go func() {
		defer close(done)

		for i := 0; i < 1000; i++ {
			lk.setSubscribeVideoQuality(fmt.Sprintf("track-%d", i), livekit.TrackSource_CAMERA, pub)
		}
	}()

	for i := 0; i < 1000; i++ {
		lk.onTrackMuted(pub, nil)
		lk.onTrackUnmuted(pub, nil)
	}

	<-done
	assert.Len(t, lk.remoteTrackPubs, 1000)
}

// writerFrame is frame f of a 30 fps VP8 stream: 4 packets from seq with
// picture ID pic, the first carrying a 640x480 key frame header when key.
func writerFrame(ssrc uint32, seq, pic uint16, f int, key bool) []*rtp.Packet {
	return vp8Frame(ssrc, seq, pic, uint32(90000+3000*f), 4, key, []byte{0xA0, 0xA1, 0xA2, 0xA3})
}

type countingRequester struct{ n atomic.Int32 }

func (c *countingRequester) RequestKeyframe()              { c.n.Add(1) }
func (c *countingRequester) RequestKeyframeForSSRC(uint32) { c.n.Add(1) }

// writerStream drives the LiveKit video path without a room, in virtual time:
// a track buffer registered as onTrackSubscribed registers it, into a
// WebmRecorder whose requester is the adapter, so both kinds of request reach
// its channel. A request is answered with a key frame at the first frame
// 100 ms later.
type writerStream struct {
	frames    int
	lost      int           // the frame missing packet 1, or -1
	late      time.Duration // if set, when that packet comes after its frame
	skipPicAt int           // the frame whose picture ID skips one, or -1
	deaf      bool          // the publisher never answers
	ignore    int           // how many requests LiveKit's PLI throttle drops first
	unasked   int           // a frame sent as a key frame for another subscriber
	stallAt   int           // if stall is set, the first frame it delays
	stall     time.Duration
	requester interfaces.KeyframeRequester
}

func (s writerStream) run(t *testing.T) (requests []time.Duration, written int) {
	synctest.Test(t, func(t *testing.T) {
		const ssrc = 1234
		rec := recorder.NewWebmRecorder(filepath.Join(t.TempDir(), "video.webm"), 0o600, 256, 32, true, false)
		rec.SetHasVideo(true)
		w := &LiveKitWebRTC{
			ctx:                 context.WithValue(context.Background(), "session", "test-session"), //nolint:staticcheck // SA1029: the key livekit.go reads
			cfg:                 (&config.Config{}).GetDefaults().LiveKit,
			rec:                 rec,
			pliStats:            map[uint32]pliTracker{},
			jitterBuffers:       map[string]*trackBuffer{},
			videoBuffers:        map[uint32]*trackBuffer{},
			keyframeRequestChan: make(chan uint32, 100),
			requestKeyframeCtx:  context.Background(),
		}
		b := newTrackBuffer(&codecs.VP8Packet{}, videoJitterLatency, lossKeyframeInterval, func() {
			w.queueKeyframeRequest(ssrc, "packet_drop")
		})
		defer b.close()
		w.registerTrackBuffer("video", true, ssrc, b)
		if s.requester != nil {
			rec.SetKeyframeRequester(s.requester)
		}

		start := time.Now()
		drain := func() bool {
			asked := false
			for len(w.keyframeRequestChan) > 0 {
				<-w.keyframeRequestChan
				requests = append(requests, time.Since(start))
				asked = true
			}

			return asked
		}
		push := func(p *rtp.Packet) {
			for _, out := range b.push(p) {
				rec.PushVideo(out)
			}
		}

		var keyAt time.Time
		var held *rtp.Packet
		var heldAt time.Time
		ignored := 0
		seq, pic := uint16(1000), uint16(100)
		for f := range s.frames {
			at := start.Add(time.Duration(f) * time.Second / 30)
			if s.stall > 0 && f >= s.stallAt {
				at = at.Add(s.stall)
			}
			time.Sleep(time.Until(at))
			// The track buffer's timer runs first at an instant it shares with a frame.
			synctest.Wait()
			if drain() && keyAt.IsZero() && !s.deaf {
				if ignored < s.ignore {
					ignored++
				} else {
					keyAt = time.Now().Add(100 * time.Millisecond)
				}
			}
			if held != nil && !time.Now().Before(heldAt) {
				push(held)
				held = nil
			}

			key := f == 0 || f == s.unasked || (!keyAt.IsZero() && !time.Now().Before(keyAt))
			if key {
				keyAt = time.Time{}
			}
			if f == s.skipPicAt {
				pic++
			}

			for j, p := range writerFrame(ssrc, seq, pic&0x7fff, f, key) {
				if f == s.lost && j == 1 {
					if s.late > 0 {
						held, heldAt = p, time.Now().Add(s.late)
					}

					continue
				}
				push(p)
			}
			seq += 4
			pic++
		}
		drain()
		written = rec.GetStats().Video.WrittenSamples
		rec.Close()
	})

	return requests, written
}

// The writer asks for a key frame when the jitter buffer releases the frames
// after a loss, 2 s late. The track buffer asked 200 ms after the loss, and
// that key frame follows those frames: a second request would bring another.
func TestWriterLeavesLossRequestToTrackBuffer(t *testing.T) {
	// How long after the track buffer's request the writer skips its own.
	window := videoJitterLatency + lossReportSlack + (&config.Config{}).GetDefaults().LiveKit.PacketReadTimeout

	t.Run("unrepaired loss", func(t *testing.T) {
		t.Parallel()
		requests, written := writerStream{frames: 105, lost: 30, skipPicAt: -1}.run(t)
		if assert.Len(t, requests, 1, "requests at %v", requests) {
			assert.Less(t, requests[0], 1400*time.Millisecond, "the track buffer's")
		}
		assert.Greater(t, written, 90, "recording resumes at the early key frame")
	})

	t.Run("request dropped", func(t *testing.T) {
		t.Parallel()
		requests, written := writerStream{frames: 130, lost: 30, skipPicAt: -1, ignore: 1}.run(t)
		if assert.Len(t, requests, 2, "requests at %v", requests) {
			assert.Less(t, requests[1]-requests[0], lossKeyframeInterval+200*time.Millisecond, "the track buffer asks again")
		}
		assert.Greater(t, written, 80, "recording resumes at the second request's key frame")
	})

	t.Run("request dropped after another subscriber's key frame", func(t *testing.T) {
		t.Parallel()
		// That key frame comes between the loss and the request.
		requests, written := writerStream{frames: 130, lost: 30, skipPicAt: -1, ignore: 1, unasked: 33}.run(t)
		assert.Len(t, requests, 1, "requests at %v", requests)
		assert.Greater(t, written, 120, "recording resumes at that key frame")
	})

	t.Run("frames behind the loss wait for a push", func(t *testing.T) {
		t.Parallel()
		// The jitter buffer releases them at 3 s, and the next packet comes
		// 350 ms later, as at a low frame rate: past the latency and slack
		// after the request, within the read timeout.
		requests, _ := writerStream{frames: 110, lost: 30, skipPicAt: -1, stallAt: 90, stall: 350 * time.Millisecond}.run(t)
		if assert.Len(t, requests, 1, "requests at %v", requests) {
			assert.Less(t, requests[0], 1400*time.Millisecond, "the track buffer's")
		}
	})

	t.Run("picture ID gap after that key frame", func(t *testing.T) {
		t.Parallel()
		// The loss is repaired only after the track buffer's request, whose
		// key frame is written before the gap at 2 s.
		requests, _ := writerStream{frames: 66, lost: 30, late: 300 * time.Millisecond, skipPicAt: 60}.run(t)
		if assert.Len(t, requests, 2, "requests at %v", requests) {
			assert.Less(t, requests[0], 1400*time.Millisecond, "the track buffer's")
			assert.GreaterOrEqual(t, requests[1], 1950*time.Millisecond, "the writer's")
		}
	})

	t.Run("picture ID gap after that key frame, late in the window", func(t *testing.T) {
		t.Parallel()
		// The gap at 3.5 s falls in the part of the window that only the
		// read timeout adds.
		requests, _ := writerStream{frames: 115, lost: 30, late: 300 * time.Millisecond, skipPicAt: 105}.run(t)
		if assert.Len(t, requests, 2, "requests at %v", requests) {
			assert.Greater(t, requests[1]-requests[0], videoJitterLatency+lossReportSlack)
			assert.Less(t, requests[1]-requests[0], window)
		}
	})

	t.Run("key frame never comes", func(t *testing.T) {
		t.Parallel()
		requests, _ := writerStream{frames: 130, lost: 30, skipPicAt: -1, deaf: true}.run(t)
		// The track buffer asks again each second, and the writer holds back.
		assert.GreaterOrEqual(t, len(requests), 3, "requests at %v", requests)
		for i := 1; i < len(requests); i++ {
			assert.Greater(t, requests[i]-requests[i-1], lossKeyframeInterval-100*time.Millisecond, "requests at %v", requests)
			assert.Less(t, requests[i]-requests[i-1], lossKeyframeInterval+200*time.Millisecond, "requests at %v", requests)
		}
	})

	t.Run("picture ID gap and no track buffer request", func(t *testing.T) {
		t.Parallel()
		requests, _ := writerStream{frames: 20, lost: -1, skipPicAt: 10}.run(t)
		assert.Len(t, requests, 1, "requests at %v", requests)
	})

	t.Run("requester without track buffers", func(t *testing.T) {
		t.Parallel()
		// The mediasoup adapter: nothing reports its requests to the writer.
		var c countingRequester
		requests, _ := writerStream{frames: 20, lost: -1, skipPicAt: 10, requester: &c}.run(t)
		assert.Empty(t, requests)
		assert.Equal(t, int32(1), c.n.Load())
	})
}
