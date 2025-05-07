package recorder

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/at-wat/ebml-go/mkvcore"
	"github.com/at-wat/ebml-go/webm"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal"
	"github.com/jech/samplebuilder"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	log "github.com/sirupsen/logrus"
)

var _ Recorder = (*WebmRecorder)(nil)

const (
	opusSampleRate      = 48000
	vp8SampleRate       = 90000
	secondToNanoseconds = 1000000000
)

type VP8PartitionTracker struct {
	partitionsStarted    int
	partitionsComplete   int
	currentPartitionSize int
}

// This is more of a debugging thing than anything else
type VP8FrameInfo struct {
	startSequence uint16
	endSequence   uint16
	packets       []uint16 // seqnums packets making up this frame
	startTime     time.Time
	pictureID     uint16
	isKeyFrame    bool
	timestamp     uint32
	size          int
}

type DiscontinuityInfo struct {
	Count    int    `json:"count"`
	MinGap   uint16 `json:"minGap"`
	MaxGap   uint16 `json:"maxGap"`
	AvgGap   uint16 `json:"avgGap"`
	TotalGap uint16 `json:"totalGap"`
}

// BaseTrackStats contains metrics common to both audio and video tracks
type BaseTrackStats struct {
	StartTime           int64             `json:"startTime"`
	EndTime             int64             `json:"endTime"`
	StartPTS            int64             `json:"startPts"`
	EndPTS              int64             `json:"endPts"`
	AvgSampleDurationMs time.Duration     `json:"avgSampleDurationMs"`
	MaxSampleDurationMs time.Duration     `json:"maxSampleDurationMs"`
	TotalSamples        int               `json:"totalSamples"`
	WrittenSamples      int               `json:"writtenSamples"`
	RTPDiscontInfo      DiscontinuityInfo `json:"rtpDiscontInfo"`

	sampleDurationAcc time.Duration
}

// AudioTrackStats contains audio-specific metrics
type AudioTrackStats struct {
	BaseTrackStats
}

// VideoTrackStats contains video-specific metrics
type RecorderTrackStats struct {
	BaseTrackStats
	CorruptedFrames     int               `json:"corruptedFrames,omitempty"`
	AvgFrameSizeBytes   int               `json:"avgFrameSizeBytes,omitempty"`
	MaxFrameSizeBytes   int               `json:"maxFrameSizeBytes,omitempty"`
	KeyframeCount       int               `json:"keyframeCount,omitempty"`
	VP8PicIDDiscontInfo DiscontinuityInfo `json:"vp8PicIdDiscontInfo,omitempty"`
}

type RecorderStats struct {
	Audio *RecorderTrackStats `json:"audio,omitempty"`
	Video *RecorderTrackStats `json:"video,omitempty"`
}

type WebmRecorder struct {
	// Configuration fields
	file                  string
	fileMode              os.FileMode
	videoPacketQueueSize  uint16
	sorterPacketQueueSize int
	audioPacketQueueSize  uint16
	useCustomSampler      bool
	writeIVFCopy          bool

	// State tracking
	hasAudio      bool
	hasVideo      bool
	hasValidAudio bool
	hasValidVideo bool
	started       bool
	closed        bool

	// Synchronization
	m   sync.Mutex
	ctx context.Context

	// Writers and builders
	audioWriter, videoWriter       webm.BlockWriteCloser
	audioBuilder, videoBuilder     *samplebuilder.SampleBuilder
	audioTimestamp, videoTimestamp time.Duration

	// Keyframe tracking
	seenKeyFrame            bool // Seen at least one keyframe
	hasKeyFrame             bool // Has a valid keyframe to keep recording
	currKeyFrame            *rtp.Packet
	lastKeyFrameTime        time.Time
	keyframeRequester       KeyframeRequester
	lastKeyframeRequestTime time.Time

	// Frame processing
	currentFrame     []byte
	currentFrameInfo *VP8FrameInfo // Again, more debugging than anything else
	packetTimestamp  uint32
	pts              int64 // Last PTS (presentation timestamp) *written* to the WebM file

	// Sequence tracking
	lastPictureID    uint16 // Last PictureID received in any VP8 packet, not necessarily written
	lastSkippedSeq   uint16
	skipSignaled     bool
	lastProcessedSeq uint16
	expectedNextSeq  uint16

	// Frame statistics
	corruptedFrameCount int
	lastFrameSize       int
	frameTimeout        time.Duration
	frameStartTime      time.Time
	maxFrameSize        int
	vp8Tracker          VP8PartitionTracker
	ivfWriter           *IVFWriter

	// Stats tracking
	stats        RecorderStats
	lastAudioPTS int64
	lastVideoPTS int64
}

func NewWebmRecorder(
	file string,
	fileMode os.FileMode,
	videoPacketQueueSize uint16,
	audioPacketQueueSize uint16,
	useCustomSampler bool,
	writeIVFCopy bool,
) *WebmRecorder {
	r := &WebmRecorder{
		ctx:                   context.Background(),
		file:                  file,
		fileMode:              fileMode,
		videoPacketQueueSize:  videoPacketQueueSize,
		sorterPacketQueueSize: int(videoPacketQueueSize) + 16,
		audioPacketQueueSize:  audioPacketQueueSize,
		useCustomSampler:      useCustomSampler,
		writeIVFCopy:          writeIVFCopy,
		audioBuilder:          samplebuilder.New(audioPacketQueueSize, &codecs.OpusPacket{}, opusSampleRate),
		videoBuilder:          samplebuilder.New(videoPacketQueueSize, &codecs.VP8Packet{}, vp8SampleRate),
		lastKeyFrameTime:      time.Now(),
		// TODO Make this configurable or remove the timeout altogether - prlanzarin
		frameTimeout: time.Millisecond * 2000,
		// maxFrameSize = 5MB (basically disabled for now)
		maxFrameSize:  10 * 1024 * 1024,
		skipSignaled:  false,
		hasAudio:      false,
		hasVideo:      false,
		hasValidAudio: false,
		hasValidVideo: false,
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

func (r *WebmRecorder) SetHasVideo(hasVideo bool) {
	r.hasVideo = hasVideo
}

func (r *WebmRecorder) GetHasVideo() bool {
	return r.hasVideo
}

func (r *WebmRecorder) SetKeyframeRequester(requester KeyframeRequester) {
	r.m.Lock()
	defer r.m.Unlock()
	r.keyframeRequester = requester
	r.lastKeyframeRequestTime = time.Time{}
}

func (r *WebmRecorder) VideoTimestamp() time.Duration {
	return r.videoTimestamp
}

func (r *WebmRecorder) AudioTimestamp() time.Duration {
	return r.audioTimestamp
}

func (r *WebmRecorder) GetStats() *RecorderStats {
	r.m.Lock()
	defer r.m.Unlock()

	stats := r.stats

	if stats.Audio != nil {
		stats.Audio.EndTime = time.Now().Unix()
		stats.Audio.EndPTS = r.lastAudioPTS

		if stats.Audio.TotalSamples > 0 {
			stats.Audio.AvgSampleDurationMs = stats.Audio.sampleDurationAcc /
				time.Duration(stats.Audio.TotalSamples)
		}

		if stats.Audio.RTPDiscontInfo.Count > 0 {
			stats.Audio.RTPDiscontInfo.AvgGap = uint16(
				stats.Audio.RTPDiscontInfo.TotalGap / uint16(stats.Audio.RTPDiscontInfo.Count),
			)
		}
	}

	if stats.Video != nil {
		stats.Video.EndTime = time.Now().Unix()
		stats.Video.EndPTS = r.pts

		if stats.Video.TotalSamples > 0 {
			stats.Video.AvgFrameSizeBytes = stats.Video.AvgFrameSizeBytes / stats.Video.TotalSamples
		}

		if stats.Video.TotalSamples > 0 {
			stats.Video.AvgSampleDurationMs = stats.Video.sampleDurationAcc /
				time.Duration(stats.Video.TotalSamples)
		}

		if stats.Video.RTPDiscontInfo.Count > 0 {
			stats.Video.RTPDiscontInfo.AvgGap = uint16(
				stats.Video.RTPDiscontInfo.TotalGap / uint16(stats.Video.RTPDiscontInfo.Count),
			)
		}

		if stats.Video.VP8PicIDDiscontInfo.Count > 0 {
			stats.Video.VP8PicIDDiscontInfo.AvgGap = uint16(
				stats.Video.VP8PicIDDiscontInfo.TotalGap / uint16(stats.Video.VP8PicIDDiscontInfo.Count),
			)
		}
	}

	return &stats
}

func (r *WebmRecorder) PushVideo(p *rtp.Packet) {
	if !r.hasVideo || p == nil {
		return
	}

	if !r.useCustomSampler {
		r.pushVP8Builtin(p)
	} else {
		r.pushVP8Custom(p)
	}
}

func (r *WebmRecorder) PushAudio(p *rtp.Packet) {
	if !r.hasAudio || p == nil {
		return
	}

	r.pushOpus(p)
}

func (r *WebmRecorder) NotifySkippedPacket(seq uint16) {
	r.m.Lock()
	defer r.m.Unlock()

	r.lastSkippedSeq = seq
	r.skipSignaled = true

	// Frame in progress and skipped packet might belong to it, discard current
	// frame to avoid stream corruption ( not that great but better than
	// nothing)
	if r.currentFrame != nil &&
		((r.currentFrameInfo != nil &&
			isSequenceInRange(seq, r.currentFrameInfo.startSequence, r.lastProcessedSeq)) ||
			r.currentFrameInfo == nil) {
		var logMsgPkts = "unknown"

		if r.currentFrameInfo != nil {
			logMsgPkts = fmt.Sprintf("%d-%d (%d)",
				r.currentFrameInfo.startSequence,
				r.lastProcessedSeq,
				len(r.currentFrameInfo.packets),
			)
		}

		log.WithField("session", r.ctx.Value("session")).
			Warnf("Discarding frame due to skipped packet: seq=%d, frame=%v", seq, logMsgPkts)

		r.currentFrame = nil
		r.currentFrameInfo = nil
		r.hasKeyFrame = false
		r.RequestKeyframe()
	}
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

	if r.ivfWriter != nil {
		if err := r.ivfWriter.Close(); err != nil {
			log.WithField("session", r.ctx.Value("session")).
				Warnf("Error closing IVF writer: %v", err)
		} else {
			log.WithField("session", r.ctx.Value("session")).
				Debugf("IVF writer closed")
		}

		r.ivfWriter = nil
	}

	return r.videoTimestamp
}

func (r *WebmRecorder) Close() time.Duration {
	r.m.Lock()
	ts := r.close()
	r.m.Unlock()

	return ts
}

func (r *WebmRecorder) initVideoStats() {
	if r.stats.Video == nil {
		r.stats.Video = &RecorderTrackStats{
			BaseTrackStats: BaseTrackStats{
				StartTime: time.Now().Unix(),
				StartPTS:  r.lastVideoPTS,
				RTPDiscontInfo: DiscontinuityInfo{
					Count:    0,
					MinGap:   0,
					MaxGap:   0,
					TotalGap: 0,
				},
			},
			VP8PicIDDiscontInfo: DiscontinuityInfo{
				Count:    0,
				MinGap:   0,
				MaxGap:   0,
				TotalGap: 0,
			},
		}
	}
}

func (r *WebmRecorder) initAudioStats() {
	if r.stats.Audio == nil {
		r.stats.Audio = &RecorderTrackStats{
			BaseTrackStats: BaseTrackStats{
				StartTime: time.Now().Unix(),
				StartPTS:  r.lastAudioPTS,
				RTPDiscontInfo: DiscontinuityInfo{
					Count:    0,
					MinGap:   0,
					MaxGap:   0,
					TotalGap: 0,
				},
			},
		}
	}
}

func (r *WebmRecorder) pushVP8Builtin(packet *rtp.Packet) {
	if !r.hasVideo {
		return
	}

	r.m.Lock()
	defer r.m.Unlock()

	if len(packet.Payload) == 0 || r.closed {
		return
	}

	r.initVideoStats()
	isKeyFrame := IsVP8KeyFrame(packet)

	if isKeyFrame {
		r.currKeyFrame = packet
		r.lastKeyFrameTime = time.Now()
	}

	if r.expectedNextSeq > 0 && packet.SequenceNumber != r.expectedNextSeq {
		gap := calculateSequenceGap(packet.SequenceNumber, r.expectedNextSeq)
		r.trackRTPDiscontinuity(&r.stats.Video.BaseTrackStats, gap)

		log.WithField("session", r.ctx.Value("session")).
			Debugf("Video sequence discontinuity detected: expected=%d, got=%d, gap=%d",
				r.expectedNextSeq, packet.SequenceNumber, gap)
	}

	r.setExpectedNextSeq(packet.SequenceNumber)
	r.videoBuilder.Push(packet)
	log.WithField("session", r.ctx.Value("session")).
		Tracef("BUILTIN: VP8 RTP-DEPAY: seq=%d, ts=%d, marker=%v, S=%v, PictureID=%d, KeyFrame=%v, size=%d, PartID=%d",
			packet.SequenceNumber, packet.Timestamp, packet.Marker, isKeyFrame,
			0, isKeyFrame, len(packet.Payload), 0)

	for {
		sample, ts := r.videoBuilder.PopWithTimestamp()

		if sample == nil {
			return
		}

		isKf := false

		if r.currKeyFrame != nil {
			isKf = ts == r.currKeyFrame.Timestamp
		}

		if isKf && ((r.videoWriter == nil && r.hasVideo) ||
			(r.audioWriter == nil && r.hasAudio) ||
			(r.hasVideo && r.hasAudio && (!r.hasValidVideo || !r.hasValidAudio))) {
			width, height := GetVP8KFDimension(packet)
			log.WithField("session", r.ctx.Value("session")).
				Tracef("Frame dimensions: %dx%d", width, height)
			r.initWriter(width, height)
		}

		// Track frame stats when frame is complete, regardless of whether it's written
		duration := sample.Duration
		r.trackFrameStats(r.stats.Video, len(sample.Data), isKf, duration)

		if r.videoWriter != nil {
			r.videoTimestamp += duration
			log.WithField("session", r.ctx.Value("session")).
				Tracef("Writing VP8 frame: ts=%d, size=%d, KF=%v", ts, len(sample.Data), isKf)

			if _, err := r.videoWriter.Write(isKf, int64(r.videoTimestamp/time.Millisecond), sample.Data); err != nil {
				log.WithField("session", r.ctx.Value("session")).
					Errorf("Error writing video frame: %v", err)
				r.hasKeyFrame = false
				r.RequestKeyframe()
			} else {
				r.stats.Video.WrittenSamples++
				log.WithField("session", r.ctx.Value("session")).
					Tracef("VP8 frame written: ts=%d, size=%d, KF=%v", ts, len(sample.Data), isKf)
			}
		}

		if r.writeIVFCopy {
			r.pushToIVFWriter(sample.Data, packet)
		}
	}
}

func (r *WebmRecorder) setExpectedNextSeq(currentSeq uint16) {
	r.expectedNextSeq = currentSeq + 1
}

func (r *WebmRecorder) pushOpus(op *rtp.Packet) {
	defer func() {
		if err := recover(); err != nil {
			log.WithField("session", r.ctx.Value("session")).
				WithField("error", err).
				WithField("stack", string(debug.Stack())).
				WithField("packet_seq", op.SequenceNumber).
				WithField("packet_ts", op.Timestamp).
				WithField("packet_size", len(op.Payload)).
				WithField("expected_seq", r.expectedNextSeq).
				WithField("audio_timestamp", r.audioTimestamp).
				WithField("has_audio", r.hasAudio).
				WithField("has_video", r.hasVideo).
				WithField("closed", r.closed).
				WithField("samplebuilder_len", r.audioBuilder.Len()).
				Error("Panic detected in pushOpus")
			panic(err)
		}
	}()

	if !r.hasAudio {
		return
	}

	r.m.Lock()
	defer r.m.Unlock()

	if len(op.Payload) == 0 || r.closed {
		return
	}

	// Create a deep copy of the packet before passing to the recorder
	// since samplebuilder docs state that it retains the packet reference
	p := &rtp.Packet{
		Header:  op.Header,
		Payload: make([]byte, len(op.Payload)),
	}
	copy(p.Payload, op.Payload)

	r.initAudioStats()

	if r.expectedNextSeq > 0 && p.SequenceNumber != r.expectedNextSeq {
		gap := calculateSequenceGap(p.SequenceNumber, r.expectedNextSeq)
		r.trackRTPDiscontinuity(&r.stats.Audio.BaseTrackStats, gap)

		log.WithField("session", r.ctx.Value("session")).
			WithField("gap", gap).
			WithField("expected_seq", r.expectedNextSeq).
			WithField("got_seq", p.SequenceNumber).
			Warn("Audio sequence discontinuity detected")
	}

	r.setExpectedNextSeq(p.SequenceNumber)
	r.audioBuilder.Push(p)

	for {
		sample := r.audioBuilder.Pop()

		if sample == nil {
			return
		}

		// Initialize writer here only if we have audio and no video
		// Otherwise, we'll initialize it in pushVP8
		if r.audioWriter == nil && r.hasAudio && !r.hasVideo {
			r.initWriter(0, 0)
		}

		if r.audioWriter != nil {
			duration := sample.Duration
			r.trackSampleStats(&r.stats.Audio.BaseTrackStats, duration)
			r.audioTimestamp += duration

			if _, err := r.audioWriter.Write(true, int64(r.audioTimestamp/time.Millisecond), sample.Data); err != nil {
				log.WithField("session", r.ctx.Value("session")).
					WithField("error", err).
					WithField("duration", duration).
					WithField("timestamp", r.audioTimestamp).
					Error("Error writing audio frame")
				r.hasValidAudio = false
			} else {
				r.stats.Audio.WrittenSamples++
				r.hasValidAudio = true
				log.WithField("session", r.ctx.Value("session")).
					WithField("duration", duration).
					WithField("timestamp", r.audioTimestamp).
					WithField("size", len(sample.Data)).
					Trace("Audio frame written")
			}
		}
	}
}

func (r *WebmRecorder) RequestKeyframe() {
	if r.keyframeRequester == nil {
		return
	}

	shouldRequest := r.lastKeyframeRequestTime.IsZero() ||
		time.Since(r.lastKeyframeRequestTime) > time.Second

	if shouldRequest {
		r.lastKeyframeRequestTime = time.Now()
		log.WithField("session", r.ctx.Value("session")).Debug("Recorder is requesting keyframe")
		r.keyframeRequester.RequestKeyframe()
	}
}

// Some validations here are kind of duplicate from the depayloader (pushVP8)
// e.g.: keyframe detection, continuity etc
// That is intentional for now - I'm trying to figure out whether the assembly
// itself is borking things rather than packet-by-packet cheking done there.
func validateVP8Frame(frame []byte, frameInfo *VP8FrameInfo) (bool, string) {
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

	// For frames with multiple packets, check for continuity and keyframe flags
	if frameInfo != nil && len(frameInfo.packets) > 1 {
		// If the frame claims to be a keyframe, the first byte of payload should indicate it
		if frameInfo.isKeyFrame != isKeyFrame {
			return false, fmt.Sprintf("keyframe flag inconsistency: %v vs %v",
				frameInfo.isKeyFrame, isKeyFrame)
		}
	}

	return true, ""
}

func IsVP8KeyFrame(packet *rtp.Packet) bool {
	if len(packet.Payload) == 0 {
		return false
	}

	vp8Packet := codecs.VP8Packet{}

	if _, err := vp8Packet.Unmarshal(packet.Payload); err != nil {
		return false
	}

	return vp8Packet.S != 0 && (vp8Packet.Payload[0]&0x1 == 0) && vp8Packet.PID == 0
}

func GetVP8KFDimension(packet *rtp.Packet) (int, int) {
	if len(packet.Payload) == 0 {
		return 0, 0
	}

	vp8Packet := codecs.VP8Packet{}

	if _, err := vp8Packet.Unmarshal(packet.Payload); err != nil {
		return 0, 0
	}

	if vp8Packet.S != 0 || vp8Packet.PID != 0 {
		return 0, 0
	}

	raw := uint(vp8Packet.Payload[6]) | uint(vp8Packet.Payload[7])<<8 | uint(vp8Packet.Payload[8])<<16 | uint(vp8Packet.Payload[9])<<24
	width := int(raw & 0x3FFF)
	height := int((raw >> 16) & 0x3FFF)

	return width, height
}

func (r *WebmRecorder) pushVP8Custom(p *rtp.Packet) {
	if !r.hasVideo {
		return
	}

	r.m.Lock()
	defer r.m.Unlock()

	if len(p.Payload) == 0 || r.closed {
		return
	}

	r.initVideoStats()

	if r.expectedNextSeq > 0 && p.SequenceNumber != r.expectedNextSeq {
		gap := calculateSequenceGap(p.SequenceNumber, r.expectedNextSeq)
		r.trackRTPDiscontinuity(&r.stats.Video.BaseTrackStats, gap)

		if !r.skipSignaled {
			log.WithField("session", r.ctx.Value("session")).
				Debugf("Sequence discontinuity detected: expected=%d, got=%d, gap=%d",
					r.expectedNextSeq, p.SequenceNumber, gap)
		}

		if r.skipSignaled && r.lastSkippedSeq+1 == p.SequenceNumber {
			log.WithField("session", r.ctx.Value("session")).
				Debugf("Processing first packet after skip: seq=%d, last_skipped=%d",
					p.SequenceNumber, r.lastSkippedSeq)

			// Force treating this as a new frame start for safety
			if r.currentFrame != nil {
				var logMsgPkts = 0

				if r.currentFrameInfo != nil {
					logMsgPkts = len(r.currentFrameInfo.packets)
				}

				log.WithField("session", r.ctx.Value("session")).
					Warnf("Discarding partial frame after skip boundary: pkts=%v", logMsgPkts)
				r.currentFrame = nil
				r.currentFrameInfo = nil
			}
		}

		r.skipSignaled = false
	}

	r.setExpectedNextSeq(p.SequenceNumber)
	r.lastProcessedSeq = p.SequenceNumber

	vp8Packet := codecs.VP8Packet{}

	if _, err := vp8Packet.Unmarshal(p.Payload); err != nil {
		log.WithField("session", r.ctx.Value("session")).
			Debugf("Failed to unmarshal VP8 packet: seq=%d, err=%v", p.SequenceNumber, err)
		r.hasKeyFrame = false
		r.RequestKeyframe()
		return
	}

	// Lifted from LK
	isKeyFrame := vp8Packet.S != 0 && (vp8Packet.Payload[0]&0x1 == 0) && vp8Packet.PID == 0
	pictureID := vp8Packet.PictureID

	log.WithField("session", r.ctx.Value("session")).
		Tracef("VP8 RTP-DEPAY: seq=%d, ts=%d, marker=%v, S=%v, PictureID=%d, KeyFrame=%v, size=%d, PartID=%d",
			p.SequenceNumber, p.Timestamp, p.Marker, vp8Packet.S == 1,
			pictureID, isKeyFrame, len(vp8Packet.Payload), vp8Packet.PID)

	// I don't fully get this: when testing with some extreme network scenarios,
	// I got packets with S=1 and PID > 0. I have not had the time to investigate
	// further, so try and make it work but log it - prlanzarin
	if vp8Packet.S == 1 && vp8Packet.PID > 0 {
		log.WithField("session", r.ctx.Value("session")).
			Warnf("Mid-frame partition detected: seq=%d, PID=%d", p.SequenceNumber, vp8Packet.PID)
		if r.currentFrameInfo != nil {
			r.currentFrameInfo.packets = append(r.currentFrameInfo.packets, p.SequenceNumber)
			r.currentFrame = append(r.currentFrame, vp8Packet.Payload[0:]...)
			return
		}
	}

	// NEW FRAME!
	if vp8Packet.S == 1 {
		// OLD FRAME, but not complete. Discard it.
		if r.currentFrame != nil && len(r.currentFrame) > 0 {
			var logMsgPkts = "none"

			if r.currentFrameInfo != nil && len(r.currentFrameInfo.packets) > 0 {
				logMsgPkts = fmt.Sprintf("%d-%d (%d)",
					r.currentFrameInfo.packets[0],
					r.currentFrameInfo.packets[len(r.currentFrameInfo.packets)-1],
					len(r.currentFrameInfo.packets))
			}

			log.WithField("session", r.ctx.Value("session")).
				Warnf("Discarding incomplete VP8 frame: packets=%v, size=%d, elapsed=%v, new_seq=%d",
					logMsgPkts,
					len(r.currentFrame),
					time.Since(r.frameStartTime), p.SequenceNumber)

			r.currentFrame = nil
			r.currentFrameInfo = nil
			r.hasKeyFrame = false
			r.RequestKeyframe()
		}

		r.vp8Tracker.partitionsStarted++
		r.vp8Tracker.currentPartitionSize = 0
		r.frameStartTime = time.Now()

		r.currentFrameInfo = &VP8FrameInfo{
			startSequence: p.SequenceNumber,
			pictureID:     pictureID,
			isKeyFrame:    isKeyFrame,
			timestamp:     p.Timestamp,
			startTime:     r.frameStartTime,
			packets:       []uint16{p.SequenceNumber},
		}

		// Check for PictureID discontinuity
		if r.lastPictureID > 0 {
			// Picture ID should ++ and wrap around at 15 bits
			expectedID := (r.lastPictureID + 1) & 0x7FFF
			pidDiff := (pictureID - r.lastPictureID) & 0x7FFF
			if pidDiff > 1 && pidDiff < 0x7000 {
				r.trackPicIDDiscontinuity(r.stats.Video, pidDiff-1)
				log.WithField("session", r.ctx.Value("session")).
					Debugf("VP8 Picture ID gap detected: expected=%d, got=%d (missing %d frames), seq=%d",
						expectedID, pictureID, pidDiff-1, p.SequenceNumber)

				if r.currentFrame != nil {
					logPID := uint16(0)

					if r.currentFrameInfo != nil {
						logPID = r.currentFrameInfo.pictureID
					}

					log.WithField("session", r.ctx.Value("session")).
						Warnf("Discarding partial frame due to PictureID discontinuity (%d -> %d)",
							logPID, pictureID)

					r.currentFrame = nil
					r.currentFrameInfo = nil
				}

				if isKeyFrame && vp8Packet.S == 1 {
					// This is a new keyframe after discontinuity - ACCEPT IT
					log.WithField("session", r.ctx.Value("session")).
						Debugf("Accepting keyframe despite PictureID discontinuity: new picID=%d, seq=%d",
							pictureID, p.SequenceNumber)

					// Continue processing this keyframe
					r.hasKeyFrame = true
					r.lastKeyFrameTime = time.Now()

					// Still update request state to avoid unnecessary PLIs
					if r.lastKeyframeRequestTime.IsZero() {
						r.lastKeyframeRequestTime = time.Now()
					}
				} else {
					// For non-keyframes with discontinuity, reject and request keyframe
					r.hasKeyFrame = false
					r.RequestKeyframe()
					return
				}
			}
		}
	} else if r.currentFrameInfo != nil {
		// Frame in progress, add packet to it
		r.currentFrameInfo.packets = append(r.currentFrameInfo.packets, p.SequenceNumber)

		// Check for timestamp discontinuity within a frame
		if r.currentFrameInfo.timestamp != p.Timestamp {
			log.WithField("session", r.ctx.Value("session")).
				Warnf("Timestamp discontinuity in frame: expected=%d, got=%d, seq=%d",
					r.currentFrameInfo.timestamp, p.Timestamp, p.SequenceNumber)
		}
	}

	r.vp8Tracker.currentPartitionSize += len(vp8Packet.Payload)

	// Frame assembly timeout - TODO review later - prlanzarin
	if r.currentFrame != nil && time.Since(r.frameStartTime) > r.frameTimeout {
		var logMsgPkts = "unknown"

		if r.currentFrameInfo != nil {
			logMsgPkts = fmt.Sprintf("[%d-%d] (%d)",
				r.currentFrameInfo.startSequence,
				r.currentFrameInfo.packets[len(r.currentFrameInfo.packets)-1],
				len(r.currentFrameInfo.packets))
		}

		log.WithField("session", r.ctx.Value("session")).
			Warnf("Frame assembly timed out after %v: packets=%v, size=%d",
				r.frameTimeout,
				logMsgPkts,
				len(r.currentFrame),
			)

		r.currentFrame = nil
		r.currentFrameInfo = nil
		r.hasKeyFrame = false
		r.RequestKeyframe()

		return
	}

	switch {
	case !r.hasKeyFrame && !isKeyFrame:
		log.WithField("session", r.ctx.Value("session")).
			Tracef("Waiting for keyframe, dropping non-keyframe: seq=%d", p.SequenceNumber)
		if !r.seenKeyFrame {
			r.RequestKeyframe()
		}

		r.currentFrame = nil
		r.currentFrameInfo = nil
		return
	case r.currentFrame == nil && vp8Packet.S != 1:
		log.WithField("session", r.ctx.Value("session")).
			Debugf("Dropping continuation packet without start bit: seq=%d", p.SequenceNumber)
		r.currentFrameInfo = nil
		r.hasKeyFrame = false
		r.RequestKeyframe()
		return
	}

	if !r.hasKeyFrame {
		if !r.seenKeyFrame {
			log.WithField("session", r.ctx.Value("session")).
				Infof("First keyframe received: seq=%d, timestamp=%d, picID=%d",
					p.SequenceNumber, p.Timestamp, pictureID)
			r.seenKeyFrame = true
			r.packetTimestamp = p.Timestamp
		}

		r.lastKeyFrameTime = time.Now()
		log.WithField("session", r.ctx.Value("session")).
			Debugf("Unblocking keyframe received: seq=%d, timestamp=%d, picID=%d",
				p.SequenceNumber, p.Timestamp, pictureID)
	}

	if r.currentFrame != nil && len(r.currentFrame)+len(vp8Packet.Payload) > r.maxFrameSize {
		var logMsgPkts = "unknown"

		if r.currentFrameInfo != nil {
			logMsgPkts = fmt.Sprintf("[%d-%d] (%d)",
				r.currentFrameInfo.startSequence,
				r.currentFrameInfo.packets[len(r.currentFrameInfo.packets)-1],
				len(r.currentFrameInfo.packets))
		}

		log.WithField("session", r.ctx.Value("session")).
			Warnf("Frame exceeds max size (%d bytes), discarding: packets=%v",
				r.maxFrameSize,
				logMsgPkts,
			)

		r.currentFrame = nil
		r.currentFrameInfo = nil
		r.hasKeyFrame = false
		r.RequestKeyframe()

		return
	}

	r.hasKeyFrame = true
	r.currentFrame = append(r.currentFrame, vp8Packet.Payload[0:]...)

	// Not a complete frame yet, wait for more packets
	if !p.Marker || len(r.currentFrame) == 0 {
		return
	}

	if p.Marker {
		r.vp8Tracker.partitionsComplete++

		if r.currentFrameInfo != nil {
			r.currentFrameInfo.endSequence = p.SequenceNumber
			r.currentFrameInfo.size = len(r.currentFrame)
			r.lastPictureID = r.currentFrameInfo.pictureID

			log.WithField("session", r.ctx.Value("session")).
				Tracef("VP8 frame complete: seq=[%d-%d], ts=%d, packets=%d, size=%d, elapsed=%v, keyframe=%v",
					r.currentFrameInfo.startSequence, p.SequenceNumber,
					p.Timestamp, len(r.currentFrameInfo.packets), len(r.currentFrame),
					time.Since(r.currentFrameInfo.startTime), r.currentFrameInfo.isKeyFrame)
		}

		log.WithField("session", r.ctx.Value("session")).
			Tracef("VP8 partition stats: started=%d, complete=%d, lastSize=%d",
				r.vp8Tracker.partitionsStarted, r.vp8Tracker.partitionsComplete,
				r.vp8Tracker.currentPartitionSize)

		// Track frame stats when frame is complete, regardless of whether it's written
		r.trackFrameStats(r.stats.Video, len(r.currentFrame), isKeyFrame, time.Since(r.frameStartTime))
	}

	if valid, reason := validateVP8Frame(r.currentFrame, r.currentFrameInfo); !valid {
		var logMsgPkts = "unknown"

		if r.currentFrameInfo != nil {
			logMsgPkts = fmt.Sprintf("[%d-%d] (%d)",
				r.currentFrameInfo.startSequence,
				r.currentFrameInfo.packets[len(r.currentFrameInfo.packets)-1],
				len(r.currentFrameInfo.packets),
			)
		}

		log.WithField("session", r.ctx.Value("session")).
			Warnf("Discarding invalid VP8 frame: %s, packets=%v", reason, logMsgPkts)

		r.corruptedFrameCount++
		r.currentFrame = nil
		r.currentFrameInfo = nil
		r.hasKeyFrame = false
		r.RequestKeyframe()

		return
	}

	if r.currentFrameInfo != nil {
		// PictureID continuity
		if r.lastPictureID > 0 &&
			pictureID != ((r.lastPictureID+1)&0x7FFF) &&
			r.currentFrameInfo.pictureID != pictureID {
			log.WithField("session", r.ctx.Value("session")).
				Warnf("Picture ID mismatch in frame: start=%d, end=%d",
					r.currentFrameInfo.pictureID, pictureID)
		}
	}

	r.lastFrameSize = len(r.currentFrame)
	r.corruptedFrameCount = 0 // Reset corruption counter on valid frame

	frameSize := len(r.currentFrame)
	log.WithField("session", r.ctx.Value("session")).
		Tracef("Assembled complete VP8 frame: keyframe=%v, size=%d",
			isKeyFrame, frameSize)

	duration := time.Duration((float64(p.Timestamp-r.packetTimestamp)/float64(vp8SampleRate))*secondToNanoseconds) * time.Nanosecond
	newVideoTs := r.videoTimestamp + duration
	newPts := int64(newVideoTs / time.Millisecond)

	log.WithField("session", r.ctx.Value("session")).
		Tracef("Frame duration: %v, pts=%v, new timestamp: %v, prevPacketTS: %v, newPacketTS: %v",
			duration, newPts, r.videoTimestamp+duration, r.packetTimestamp, p.Timestamp)

	// Initialize writer if either:
	// 1. We don't have a video writer yet and we have video
	// 2. We don't have an audio writer yet and we have audio
	// 3. We have both tracks but haven't received valid samples from both yet
	if (r.videoWriter == nil && r.hasVideo) ||
		(r.audioWriter == nil && r.hasAudio) ||
		(r.hasVideo && r.hasAudio && (!r.hasValidVideo || !r.hasValidAudio)) {

		raw := uint(r.currentFrame[6]) | uint(r.currentFrame[7])<<8 | uint(r.currentFrame[8])<<16 | uint(r.currentFrame[9])<<24
		width := int(raw & 0x3FFF)
		height := int((raw >> 16) & 0x3FFF)

		log.WithField("session", r.ctx.Value("session")).
			Tracef("Frame dimensions: %dx%d", width, height)

		r.initWriter(width, height)
	}

	if r.videoWriter != nil {
		if r.pts > 0 && r.pts == newPts {
			log.WithField("session", r.ctx.Value("session")).
				Warnf("Duplicate frame detected: pts=%d, seq=[%d-%d], duration=%v, prevPacketTS=%d, newPacketTS=%d",
					newPts, r.currentFrameInfo.startSequence, p.SequenceNumber, duration, r.packetTimestamp, p.Timestamp)
		}

		r.videoTimestamp = newVideoTs
		r.packetTimestamp = p.Timestamp
		r.pts = newPts

		if isKeyFrame {
			r.lastKeyFrameTime = time.Now()
		}

		log.WithField("session", r.ctx.Value("session")).
			Tracef("Writing frame to WebM: pts=%d, isKey=%v, size=%d",
				newPts, isKeyFrame, frameSize)

		if _, err := r.videoWriter.Write(isKeyFrame, newPts, r.currentFrame); err != nil {
			log.WithField("session", r.ctx.Value("session")).
				Errorf("Error writing video frame: %v", err)
			r.hasKeyFrame = false
			r.RequestKeyframe()
		} else {
			var sSeq, eSeq, pktCount, stime, pictureID, timestamp int = 0, 0, 0, 0, 0, 0

			r.stats.Video.WrittenSamples++

			if r.currentFrameInfo != nil {
				sSeq = int(r.currentFrameInfo.startSequence)
				eSeq = int(r.currentFrameInfo.endSequence)
				stime = int(r.currentFrameInfo.startTime.Unix())
				pictureID = int(r.currentFrameInfo.pictureID)
				timestamp = int(r.currentFrameInfo.timestamp)
				pktCount = len(r.currentFrameInfo.packets)
			}

			log.WithField("session", r.ctx.Value("session")).
				Tracef("Written VP8 frame to WebM: size=%d, pts=%d, seq=[%d-%d], ts=%d, picID=%d, keyframe=%v, pkts=%d, stime=%d",
					len(r.currentFrame),
					newPts,
					sSeq, eSeq, timestamp, pictureID, isKeyFrame, pktCount, stime,
				)
		}

		if r.writeIVFCopy {
			r.pushToIVFWriter(r.currentFrame, p)
		}

		r.hasValidVideo = true
	}

	r.currentFrame = nil
	r.currentFrameInfo = nil
}

func (r *WebmRecorder) startIVFWriter() error {
	if r.ivfWriter == nil {
		ivf, err := NewIVFWriter(r.file)

		if err != nil {
			return fmt.Errorf("failed to create RTP ivf: %w", err)
		}

		r.ivfWriter = ivf

		log.WithField("session", r.ctx.Value("session")).
			Infof("IVF copy enabled: %s", ivf.filePath)
	}

	return nil
}

func (r *WebmRecorder) pushToIVFWriter(frame []byte, p *rtp.Packet) {
	if r.ivfWriter != nil && len(r.currentFrame) > 0 {
		isKeyFrame := false

		if r.currentFrameInfo != nil {
			isKeyFrame = r.currentFrameInfo.isKeyFrame
		} else {
			isKeyFrame = IsVP8KeyFrame(p)
		}

		if isKeyFrame && len(frame) >= 10 {
			raw := uint(frame[6]) | uint(frame[7])<<8 | uint(frame[8])<<16 | uint(frame[9])<<24
			width := uint16(raw & 0x3FFF)
			height := uint16((raw >> 16) & 0x3FFF)

			r.ivfWriter.UpdateDimensions(width, height)
		}

		err := r.ivfWriter.WriteFrame(frame, p.Timestamp, isKeyFrame)

		if err != nil {
			log.WithField("session", r.ctx.Value("session")).
				Warnf("Failed to write frame to IVF copy: %v", err)
		}
	}
}

func (r *WebmRecorder) initWriter(width, height int) {
	if !r.hasAudio && !r.hasVideo {
		log.WithField("session", r.ctx.Value("session")).
			Error("Cannot initialize writer: neither audio nor video tracks are present")
		return
	}

	log.WithField("file", r.file).WithField("fileMode", r.fileMode).Debug("Opening file for writing")
	w, err := os.OpenFile(r.file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, r.fileMode)
	if err != nil {
		panic(err)
	}

	info := &webm.Info{
		TimecodeScale: 1000000, // 1ms
		MuxingApp:     internal.AppName,
		WritingApp:    internal.AppName,
	}

	var tracks []webm.TrackEntry

	if r.hasVideo {
		// TODO: the webm writer should NOT have uneeded tracks if they are not necessary.
		// Review this - prlanzarin
		if width == 0 || height == 0 {
			width = 640
			height = 480
			log.WithField("session", r.ctx.Value("session")).
				Debug("Using default video dimensions for audio-only initialization")
		}

		tracks = append(tracks, webm.TrackEntry{
			Name:        "Video",
			TrackNumber: 1,
			TrackUID:    12345,
			CodecID:     "V_VP8",
			TrackType:   1,
			Video: &webm.Video{
				PixelWidth:  uint64(width),
				PixelHeight: uint64(height),
			},
		})
	}

	// Audio track is optional
	if r.hasAudio {
		tracks = append(tracks, webm.TrackEntry{
			Name:        "Audio",
			TrackNumber: uint64(len(tracks) + 1),
			TrackUID:    54321,
			CodecID:     "A_OPUS",
			TrackType:   2,
			Audio: &webm.Audio{
				SamplingFrequency: 48000.0,
				Channels:          2,
			},
		})
	}

	interceptor, err := mkvcore.NewMultiTrackBlockSorter(
		mkvcore.WithMaxDelayedPackets(r.sorterPacketQueueSize),
		mkvcore.WithSortRule(mkvcore.BlockSorterWriteOutdated),
	)

	if err != nil {
		// TODO review - panic is not the best choice here.
		panic(err)
	}

	writers, err := webm.NewSimpleBlockWriter(
		w,
		tracks,
		mkvcore.WithSegmentInfo(info),
		mkvcore.WithBlockInterceptor(interceptor),
	)

	if err != nil {
		// TODO review - panic is not the best choice here.
		panic(err)
	}

	log.WithField("session", r.ctx.Value("session")).
		Infof("webm writers started with video=%t, audio=%t : %s", r.hasVideo, r.hasAudio, r.file)

	writerIndex := 0

	if r.hasVideo {
		r.videoWriter = writers[writerIndex]
		writerIndex++
	}

	if r.hasAudio && writerIndex < len(writers) {
		r.audioWriter = writers[writerIndex]
	}

	r.started = true

	if r.writeIVFCopy && r.hasVideo {
		if err := r.startIVFWriter(); err != nil {
			log.WithField("session", r.ctx.Value("session")).
				Warnf("Error starting RTP ivf: %v", err)
		}
	}
}

//  ----- Stats tracking methods -----

func (r *WebmRecorder) trackRTPDiscontinuity(stats *BaseTrackStats, gap uint16) {
	if stats == nil {
		return
	}

	stats.RTPDiscontInfo.Count++
	stats.RTPDiscontInfo.TotalGap += gap

	if gap < stats.RTPDiscontInfo.MinGap || stats.RTPDiscontInfo.MinGap == 0 {
		stats.RTPDiscontInfo.MinGap = gap
	}

	if gap > stats.RTPDiscontInfo.MaxGap {
		stats.RTPDiscontInfo.MaxGap = gap
	}
}

func (r *WebmRecorder) trackPicIDDiscontinuity(stats *RecorderTrackStats, gap uint16) {
	if stats == nil {
		return
	}

	stats.VP8PicIDDiscontInfo.Count++
	stats.VP8PicIDDiscontInfo.TotalGap += gap

	if gap < stats.VP8PicIDDiscontInfo.MinGap || stats.VP8PicIDDiscontInfo.MinGap == 0 {
		stats.VP8PicIDDiscontInfo.MinGap = gap
	}

	if gap > stats.VP8PicIDDiscontInfo.MaxGap {
		stats.VP8PicIDDiscontInfo.MaxGap = gap
	}
}

func (r *WebmRecorder) trackFrameStats(stats *RecorderTrackStats, size int, isKeyFrame bool, duration time.Duration) {
	if stats == nil {
		return
	}

	r.trackSampleStats(&stats.BaseTrackStats, duration)
	stats.AvgFrameSizeBytes += size

	if size > stats.MaxFrameSizeBytes {
		stats.MaxFrameSizeBytes = size
	}

	if isKeyFrame {
		stats.KeyframeCount++
	}
}

func (r *WebmRecorder) trackSampleStats(stats *BaseTrackStats, duration time.Duration) {
	// Duration is in ns, convert to ms
	durationMs := duration / time.Millisecond

	if stats == nil {
		return
	}

	stats.TotalSamples++
	stats.sampleDurationAcc += durationMs

	if durationMs > stats.MaxSampleDurationMs {
		stats.MaxSampleDurationMs = durationMs
	}
}

//  ----- Helpers -----

func calculateSequenceGap(current, expected uint16) uint16 {
	if current >= expected {
		return current - expected
	}

	return (65535 - expected) + current + 1
}

func isSequenceInRange(seq, start, end uint16) bool {
	if start <= end {
		return seq >= start && seq <= end
	}

	return seq >= start || seq <= end
}
