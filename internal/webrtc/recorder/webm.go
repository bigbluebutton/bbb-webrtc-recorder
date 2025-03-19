package recorder

import (
	"context"
	"fmt"
	"github.com/at-wat/ebml-go/mkvcore"
	"github.com/at-wat/ebml-go/webm"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3/pkg/media/samplebuilder"
	log "github.com/sirupsen/logrus"
	"os"
	"sync"
	"time"
)

var _ Recorder = (*WebmRecorder)(nil)

const (
	vp8SampleRate       = 90000
	secondToNanoseconds = 1000000000
)

type VP8PartitionTracker struct {
	partitionsStarted int
	partitionsComplete int
	currentPartitionSize int
}

type WebmRecorder struct {
	ctx      context.Context
	file     string
	fileMode os.FileMode
	m        sync.Mutex

	audioWriter, videoWriter       webm.BlockWriteCloser
	audioBuilder, videoBuilder     *samplebuilder.SampleBuilder
	audioTimestamp, videoTimestamp time.Duration

	started bool
	closed  bool

	seenKeyFrame      bool
	currentFrame      []byte
	packetTimestamp   uint32
	hasAudio          bool

	lastKeyFrameTime     time.Time
	corruptedFrameCount  int
	lastFrameSize        int
	frameTimeout         time.Duration
	frameStartTime       time.Time
	maxFrameSize         int
	vp8Tracker           VP8PartitionTracker
}

func NewWebmRecorder(file string, fileMode os.FileMode) *WebmRecorder {
	r := &WebmRecorder{
		ctx:                 context.Background(),
		file:                file,
		fileMode:            fileMode,
		audioBuilder:        samplebuilder.New(10, &codecs.OpusPacket{}, 48000),
		lastKeyFrameTime:    time.Now(),
		frameTimeout:        time.Millisecond * 500,
		maxFrameSize:        5 * 1024 * 1024,
	}

	return r
}

func (r *WebmRecorder) GetFilePath() string {
	return r.file
}

func (r *WebmRecorder) WithContext(ctx context.Context) {
	r.ctx = ctx
}

func (r *WebmRecorder) SetHasAudio(hasAudio bool) {
	r.hasAudio = hasAudio
}

func (r *WebmRecorder) GetHasAudio() bool {
	return r.hasAudio
}

func (r *WebmRecorder) VideoTimestamp() time.Duration {
	return r.videoTimestamp
}

func (r *WebmRecorder) AudioTimestamp() time.Duration {
	return r.audioTimestamp
}

func (r *WebmRecorder) PushVideo(p *rtp.Packet) {
	r.pushVP8(p)
}

func (r *WebmRecorder) PushAudio(p *rtp.Packet) {
	r.pushOpus(p)
}

// Locked
func (r *WebmRecorder) close() time.Duration {
	if r.closed {
		return r.videoTimestamp
	}
	r.closed = true

	if r.audioWriter != nil {
		if err := r.audioWriter.Close(); err != nil {
			panic(err)
		}
	}
	if r.videoWriter != nil {
		if err := r.videoWriter.Close(); err != nil {
			panic(err)
		}
	}
	if r.started {
		log.WithField("session", r.ctx.Value("session")).
			Infof("webm writer closed: %s", r.file)
	} else {
		log.WithField("session", r.ctx.Value("session")).
			Info("webm writer closed without starting")
	}

	return r.videoTimestamp
}

func (r *WebmRecorder) Close() time.Duration {
	r.m.Lock()
	ts := r.close()
	r.m.Unlock()

	return ts
}

func (r *WebmRecorder) pushOpus(p *rtp.Packet) {
	r.m.Lock()
	defer r.m.Unlock()

	if len(p.Payload) == 0 || r.closed {
		return
	}

	r.audioBuilder.Push(p)

	for {
		sample := r.audioBuilder.Pop()
		if sample == nil {
			return
		}
		if r.audioWriter != nil {
			r.audioTimestamp += sample.Duration
			if _, err := r.audioWriter.Write(true, int64(r.audioTimestamp/time.Millisecond), sample.Data); err != nil {
				panic(err)
			}
		}
	}
}

func validateVP8Frame(frame []byte) (bool, string) {
	// TODO review min frame len - matching with VP8 header size for now
	minFrameLen := 10

	if len(frame) < minFrameLen {
		return false, "frame too small"
	}

	// VP8 frame starts with a header byte
	frameHeader := frame[0]
	isKeyFrame := (frameHeader & 0x01) == 0

	if isKeyFrame {
		// Keyframes  has to have at least a 10 byte block to be valid (RFC 6386#4)
		if len(frame) < 10 {
			return false, "keyframe too small"
		}

		// VP8 keyframe starts with 3 bytes frame tag, then dimensions (RFC 6386#9.1)
		raw := uint(frame[6]) | uint(frame[7])<<8 | uint(frame[8])<<16 | uint(frame[9])<<24
		width := int(raw & 0x3FFF)
		height := int((raw >> 16) & 0x3FFF)

		// Sanity check dimensions. Lower boundary is ok, upper boundary is
		// arbitrary. It should be enough for any reasonable use.
		// TODO review upper boundary later - prlanzarin
		if width < 16 || width > 8192 || height < 16 || height > 8192 {
			return false, fmt.Sprintf("invalid dimensions: %dx%d", width, height)
		}
	}

	return true, ""
}

func (r *WebmRecorder) pushVP8(p *rtp.Packet) {
	r.m.Lock()
	defer r.m.Unlock()

	if len(p.Payload) == 0 || r.closed {
		return
	}

	vp8Packet := codecs.VP8Packet{}
	if _, err := vp8Packet.Unmarshal(p.Payload); err != nil {
		log.WithField("session", r.ctx.Value("session")).
			Debugf("Failed to unmarshal VP8 packet: seq=%d, err=%v", p.SequenceNumber, err)
		return
	}

	isKeyFrame := vp8Packet.Payload[0]&0x01 == 0

	if isKeyFrame {
		log.WithField("session", r.ctx.Value("session")).
			Tracef("Received keyframe: seq=%d, timestamp=%d, size=%d",
				p.SequenceNumber, p.Timestamp, len(p.Payload))
	}

	if vp8Packet.S == 1 {
		r.vp8Tracker.partitionsStarted++
		r.vp8Tracker.currentPartitionSize = 0

		// If we have an existing frame being assembled that wasn't properly finished
		// with a marker bit, discard it - it's likely corrupted
		if r.currentFrame != nil && len(r.currentFrame) > 0 {
			log.WithField("session", r.ctx.Value("session")).
				Warn("Discarding incomplete frame as new frame started")
			r.currentFrame = nil
		}

		r.frameStartTime = time.Now()
	}

	r.vp8Tracker.currentPartitionSize += len(vp8Packet.Payload)

	// Frame assembly timeout check - prevent stale frame assembly
	if r.currentFrame != nil && time.Since(r.frameStartTime) > r.frameTimeout {
		log.WithField("session", r.ctx.Value("session")).
			Warnf("Frame assembly timed out after %v, discarding partial frame", r.frameTimeout)
		r.currentFrame = nil
	}

	switch {
	case !r.seenKeyFrame && !isKeyFrame:
		// Still waiting for first keyframe
		log.WithField("session", r.ctx.Value("session")).
			Debug("Waiting for initial keyframe, dropping non-keyframe")
		return
	case r.currentFrame == nil && vp8Packet.S != 1:
		// Received continuation packet without a frame start
		log.WithField("session", r.ctx.Value("session")).
			Debug("Dropping continuation packet without start bit (S=1)")
		return
	}

	if !r.seenKeyFrame {
		r.packetTimestamp = p.Timestamp
		r.lastKeyFrameTime = time.Now()
		log.WithField("session", r.ctx.Value("session")).
			Infof("First keyframe received: seq=%d, timestamp=%d",
				p.SequenceNumber, p.Timestamp)
	}

	if r.currentFrame != nil && len(r.currentFrame)+len(vp8Packet.Payload) > r.maxFrameSize {
		log.WithField("session", r.ctx.Value("session")).
			Warnf("Frame exceeds max size (%d bytes), discarding", r.maxFrameSize)
		r.currentFrame = nil
		return
	}

	log.WithField("session", r.ctx.Value("session")).
		Tracef("VP8 packet: seq=%d, S=%v, marker=%v, size=%d, timestamp=%d",
			p.SequenceNumber, vp8Packet.S == 1, p.Marker, len(vp8Packet.Payload), p.Timestamp)

	r.seenKeyFrame = true
	r.currentFrame = append(r.currentFrame, vp8Packet.Payload[0:]...)

	// Not the last packet of the frame yet
	if !p.Marker || len(r.currentFrame) == 0 {
		return
	}

	if p.Marker {
		r.vp8Tracker.partitionsComplete++
		log.WithField("session", r.ctx.Value("session")).
			Tracef("VP8 partition stats: started=%d, complete=%d, lastSize=%d",
				r.vp8Tracker.partitionsStarted, r.vp8Tracker.partitionsComplete,
				r.vp8Tracker.currentPartitionSize)
	}

	if valid, reason := validateVP8Frame(r.currentFrame); !valid {
		log.WithField("session", r.ctx.Value("session")).
			Warnf("Discarding invalid VP8 frame: %s", reason)
		r.corruptedFrameCount++
		r.currentFrame = nil

		// TODO request PLI upstream
		//if r.corruptedFrameCount >= 5 && time.Since(r.lastKeyFrameTime) > time.Second*1 {
		//	log.WithField("session", r.ctx.Value("session")).
		//		Warn("Multiple corrupt frames detected, requesting keyframe")
		//	r.lastKeyFrameTime = time.Now()
		//}

		return
	}

	r.lastFrameSize = len(r.currentFrame)
	r.corruptedFrameCount = 0 // Reset corruption counter on valid frame

	frameSize := len(r.currentFrame)
	log.WithField("session", r.ctx.Value("session")).
		Tracef("Assembled complete VP8 frame: isKF=%v, size=%d",
			isKeyFrame, frameSize)

	duration := time.Duration((float64(p.Timestamp-r.packetTimestamp)/float64(vp8SampleRate))*secondToNanoseconds) * time.Nanosecond

	log.WithField("session", r.ctx.Value("session")).
		Tracef("Frame duration: %v, new timestamp: %v",
			duration, r.videoTimestamp+duration)

	if r.videoWriter == nil || (r.audioWriter == nil && r.hasAudio) {
		raw := uint(r.currentFrame[6]) | uint(r.currentFrame[7])<<8 | uint(r.currentFrame[8])<<16 | uint(r.currentFrame[9])<<24
		width := int(raw & 0x3FFF)
		height := int((raw >> 16) & 0x3FFF)

		log.WithField("session", r.ctx.Value("session")).
			Tracef("Frame dimensions: %dx%d", width, height)

		r.initWriter(width, height)
	}

	if r.videoWriter != nil {
		r.videoTimestamp += duration

		if isKeyFrame {
			r.lastKeyFrameTime = time.Now()
		}

		log.WithField("session", r.ctx.Value("session")).
			Tracef("Writing frame to WebM: pts=%d, isKey=%v, size=%d",
				int64(r.videoTimestamp/time.Millisecond), isKeyFrame, frameSize)

		if _, err := r.videoWriter.Write(isKeyFrame, int64(r.videoTimestamp/time.Millisecond), r.currentFrame); err != nil {
			log.WithField("session", r.ctx.Value("session")).
				Errorf("Error writing video frame: %v", err)
		} else {
			if isKeyFrame {
				log.WithField("session", r.ctx.Value("session")).
					Debugf("Written keyframe of size %d bytes at pts=%d", len(r.currentFrame), int64(r.videoTimestamp/time.Millisecond))
			}
		}
	}

	r.currentFrame = nil
	r.packetTimestamp = p.Timestamp
}

func (r *WebmRecorder) initWriter(width, height int) {
	w, err := os.OpenFile(r.file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, r.fileMode)
	if err != nil {
		panic(err)
	}

	info := &webm.Info{
		TimecodeScale: 1000000, // 1ms
		MuxingApp:     internal.AppName,
		WritingApp:    internal.AppName,
	}

	// Video track is always present - initialize tracks a array with it
	tracks := []webm.TrackEntry{
		{
			Name:        "Video",
			TrackNumber: 1,
			TrackUID:    12345,
			CodecID:     "V_VP8",
			TrackType:   1,
			Video: &webm.Video{
				PixelWidth:  uint64(width),
				PixelHeight: uint64(height),
			},
		},
	}

	// Audio track is optional
	if r.hasAudio {
		tracks = append(tracks, webm.TrackEntry{
			Name:        "Audio",
			TrackNumber: 2,
			TrackUID:    54321,
			CodecID:     "A_OPUS",
			TrackType:   2,
			Audio: &webm.Audio{
				SamplingFrequency: 48000.0,
				Channels:          2,
			},
		})
	}

	writers, err := webm.NewSimpleBlockWriter(w, tracks, mkvcore.WithSegmentInfo(info))

	if err != nil {
		panic(err)
	}
	log.WithField("session", r.ctx.Value("session")).
		Infof("webm writers started with %dx%d video, audio=%t : %s", width, height, r.hasAudio, r.file)
	r.videoWriter = writers[0]
	if r.hasAudio && len(writers) > 1 {
		r.audioWriter = writers[1]
	}
	r.started = true
}
