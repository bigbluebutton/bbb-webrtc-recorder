package recorder

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/at-wat/ebml-go/webm"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/interfaces"
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

	// The samplebuilder subtracts timestamps unsigned, so an Opus frame
	// whose timestamp goes back comes out this long or longer.
	audioStepBack = time.Duration(1<<31) * time.Second / opusSampleRate
)

type SequenceTracker struct {
	expectedNextSeq uint16
	kind            string
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

	// The video track's codec, the container, and the file name without
	// the extension that the container sets.
	videoCodec *Codec
	format     Format
	base       string

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
	// fileMu guards file for GetFilePath, which cannot take m: the LiveKit
	// adapter calls it holding its own lock, and m is taken before that
	// lock. SetFormat writes file under both.
	fileMu sync.Mutex

	// Writers and builders
	audioWriter, videoWriter       webm.BlockWriteCloser
	audioBuilder, videoBuilder     *samplebuilder.SampleBuilder
	audioTimestamp, videoTimestamp time.Duration
	span                           mediaSpan

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

	// For codecs that take samples: the timestamp of the last frame, written
	// or not, and whether packets pushed one at a time were warned about.
	framesSeen      bool
	lastFrameTS     uint32
	warnedPerPacket bool
	// The kinds of message already logged at Warn; see logLevel.
	warned []any
	// The RTP ticks from the first key frame to the last frame written.
	videoTicks uint64
	// When the last frame written arrived.
	lastWrittenAt time.Time

	// Sequence tracking
	lastPictureID    uint16 // Last PictureID received in any VP8 packet, not necessarily written
	lastSkippedSeq   uint16
	skipSignaled     bool
	lastProcessedSeq uint16
	// Next seqnum tracking per codec - to handle audio/video interleaving better
	// The above (with the exception of lastPictureID) should be moved to this
	// struct eventually - prlanzarin
	videoSeqTracker *SequenceTracker
	audioSeqTracker *SequenceTracker

	// Frame statistics
	corruptedFrameCount int
	lastFrameSize       int
	frameTimeout        time.Duration
	frameStartTime      time.Time
	maxFrameSize        int
	vp8Tracker          VP8PartitionTracker
	ivfWriter           *IVFWriter

	// Stats tracking
	stats        types.RecorderStats
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
	format, base := formatOf(file)
	r := &WebmRecorder{
		ctx:                   context.Background(),
		file:                  file,
		videoCodec:            CodecVP8,
		format:                format,
		base:                  base,
		fileMode:              fileMode,
		videoPacketQueueSize:  videoPacketQueueSize,
		sorterPacketQueueSize: int(videoPacketQueueSize) + 16,
		audioPacketQueueSize:  audioPacketQueueSize,
		useCustomSampler:      useCustomSampler,
		writeIVFCopy:          writeIVFCopy,
		audioBuilder:          samplebuilder.New(audioPacketQueueSize, &codecs.OpusPacket{}, opusSampleRate),
		videoBuilder:          samplebuilder.New(videoPacketQueueSize, &codecs.VP8Packet{}, vp8SampleRate),
		videoSeqTracker:       &SequenceTracker{expectedNextSeq: 0, kind: "video"},
		audioSeqTracker:       &SequenceTracker{expectedNextSeq: 0, kind: "audio"},
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
	r.fileMu.Lock()
	defer r.fileMu.Unlock()

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

func (r *WebmRecorder) GetStats() *types.RecorderStats {
	r.m.Lock()
	defer r.m.Unlock()

	stats := r.stats

	if stats.Audio != nil {
		stats.Audio.EndTime = time.Now().Unix()
		stats.Audio.EndPTS = r.lastAudioPTS

		if stats.Audio.TotalSamples > 0 {
			stats.Audio.AvgSampleDurationMs = stats.Audio.SampleDurationAcc /
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
			stats.Video.AvgSampleDurationMs = stats.Video.SampleDurationAcc /
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
	r.PushVideoAt(p, time.Now())
}

// PushVideoAt pushes p, which arrived at at.
func (r *WebmRecorder) PushVideoAt(p *rtp.Packet, at time.Time) {
	if !r.hasVideo || p == nil {
		return
	}

	r.m.Lock()
	codec := r.videoCodec
	warn := codec.frames != nil && !r.warnedPerPacket
	r.warnedPerPacket = r.warnedPerPacket || warn
	r.m.Unlock()

	if warn {
		log.WithField("session", r.ctx.Value("session")).
			Warnf("Dropping %s packets pushed one at a time: the codec takes samples", codec.MimeType)
	}

	if codec.frames != nil {
		return
	}

	if !r.useCustomSampler {
		r.pushVP8Builtin(p, at)
	} else {
		r.pushVP8Custom(p, at)
	}
}

func (r *WebmRecorder) PushAudio(p *rtp.Packet) {
	r.PushAudioAt(p, time.Now())
}

// PushAudioAt pushes p, which arrived at at.
func (r *WebmRecorder) PushAudioAt(p *rtp.Packet, at time.Time) {
	if !r.hasAudio || p == nil {
		return
	}

	r.pushOpus(p, at)
}

func (r *WebmRecorder) MediaSpan() (types.MediaSpan, bool) {
	r.m.Lock()
	defer r.m.Unlock()

	return r.span.span()
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

	// Write the audio frames still held in the samplebuilder, e.g. after a
	// packet loss: no newer packets will come to release them.
	if r.hasAudio {
		for {
			sample, ts := r.audioBuilder.ForcePopWithTimestamp()
			if sample == nil {
				break
			}

			r.writeAudioFrame(sample.Data, sample.Duration, ts)
		}
	}

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
			Infof("%s writer closed: %s", r.format, r.file)
	} else {
		log.WithField("session", r.ctx.Value("session")).
			Infof("%s writer closed without starting", r.format)
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
		r.stats.Video = &types.RecorderTrackStats{
			BaseTrackStats: types.BaseTrackStats{
				StartTime: time.Now().Unix(),
				StartPTS:  r.lastVideoPTS,
				RTPDiscontInfo: types.DiscontinuityInfo{
					Count:    0,
					MinGap:   0,
					MaxGap:   0,
					TotalGap: 0,
				},
			},
			VP8PicIDDiscontInfo: types.DiscontinuityInfo{
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
		r.stats.Audio = &types.RecorderTrackStats{
			BaseTrackStats: types.BaseTrackStats{
				StartTime: time.Now().Unix(),
				StartPTS:  r.lastAudioPTS,
				RTPDiscontInfo: types.DiscontinuityInfo{
					Count:    0,
					MinGap:   0,
					MaxGap:   0,
					TotalGap: 0,
				},
			},
		}
	}
}

func (r *WebmRecorder) setExpectedNextSeq(currentSeq uint16, kind string) {
	if kind == r.videoSeqTracker.kind {
		r.videoSeqTracker.expectedNextSeq = currentSeq + 1
	} else if kind == r.audioSeqTracker.kind {
		r.audioSeqTracker.expectedNextSeq = currentSeq + 1
	}
}

func (r *WebmRecorder) pushOpus(op *rtp.Packet, at time.Time) {
	defer func() {
		if err := recover(); err != nil {
			log.WithField("session", r.ctx.Value("session")).
				WithField("error", err).
				WithField("stack", string(debug.Stack())).
				WithField("packet_seq", op.SequenceNumber).
				WithField("packet_ts", op.Timestamp).
				WithField("packet_size", len(op.Payload)).
				WithField("expected_seq", r.audioSeqTracker.expectedNextSeq).
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

	if r.audioSeqTracker.expectedNextSeq > 0 && p.SequenceNumber != r.audioSeqTracker.expectedNextSeq {
		gap := calculateSequenceGap(p.SequenceNumber, r.audioSeqTracker.expectedNextSeq)
		r.trackRTPDiscontinuity(&r.stats.Audio.BaseTrackStats, gap)

		log.WithField("session", r.ctx.Value("session")).
			WithField("gap", gap).
			WithField("expected_seq", r.audioSeqTracker.expectedNextSeq).
			WithField("got_seq", p.SequenceNumber).
			Debug("Audio sequence discontinuity detected")
	}

	r.setExpectedNextSeq(p.SequenceNumber, "audio")
	r.span.arrived(p.Timestamp, at)
	r.audioBuilder.Push(p)

	for {
		sample, ts := r.audioBuilder.PopWithTimestamp()

		if sample == nil {
			return
		}

		r.writeAudioFrame(sample.Data, sample.Duration, ts)
	}
}

// Locked. duration is the time since the previous frame.
func (r *WebmRecorder) writeAudioFrame(data []byte, duration time.Duration, ts uint32) {
	// Initialize writer here only if we have audio and no video
	// Otherwise, we'll initialize it in pushVP8
	if r.audioWriter == nil && r.hasAudio && !r.hasVideo {
		r.initWriter(0, 0)
	}

	if r.audioWriter != nil {
		// Writing it would move every later frame a day ahead.
		if duration >= audioStepBack {
			log.WithField("session", r.ctx.Value("session")).
				WithField("duration", duration).
				WithField("timestamp", r.audioTimestamp).
				Warn("Skipping audio frame whose timestamp goes back")

			return
		}

		r.trackSampleStats(&r.stats.Audio.BaseTrackStats, duration)
		r.audioTimestamp += duration
		pts := int64(r.audioTimestamp / time.Millisecond)

		// LiveKit's blank frames at downtrack close can carry the previous
		// frame's RTP timestamp plus one tick: same ms, overlapping audio.
		if r.stats.Audio.WrittenSamples > 0 && pts <= r.lastAudioPTS {
			log.WithField("session", r.ctx.Value("session")).
				WithField("duration", duration).
				WithField("timestamp", r.audioTimestamp).
				Debug("Skipping audio frame that does not advance the timestamp")

			return
		}

		if _, err := r.audioWriter.Write(true, pts, data); err != nil {
			log.WithField("session", r.ctx.Value("session")).
				WithField("error", err).
				WithField("duration", duration).
				WithField("timestamp", r.audioTimestamp).
				Error("Error writing audio frame")
			r.hasValidAudio = false
		} else {
			r.stats.Audio.WrittenSamples++
			r.lastAudioPTS = pts
			r.hasValidAudio = true
			r.span.wrote(ts, time.Duration(pts)*time.Millisecond, duration, audioSpan)
			log.WithField("session", r.ctx.Value("session")).
				WithField("duration", duration).
				WithField("timestamp", r.audioTimestamp).
				WithField("size", len(data)).
				Trace("Audio frame written")
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

// keyframeDue reports whether the requester already asked for a key frame
// since the last one arrived.
func (r *WebmRecorder) keyframeDue(ssrc uint32) bool {
	t, ok := r.keyframeRequester.(interfaces.KeyframeRequestTracker)

	return ok && t.KeyframeRequestedSince(ssrc, r.lastKeyFrameTime)
}

func (r *WebmRecorder) initWriter(width, height int) {
	r.openWriter(width, height, nil)
}

// openWriter creates the file with a track for each kind of media r has.
// The video track is in r's video codec and carries codecPrivate. It is
// sized width x height, or 640x480 when either is 0. The audio track is
// Opus.
func (r *WebmRecorder) openWriter(width, height int, codecPrivate []byte) {
	if !r.hasAudio && !r.hasVideo {
		log.WithField("session", r.ctx.Value("session")).
			Error("Cannot initialize writer: neither audio nor video tracks are present")
		return
	}

	var tracks []trackConfig

	if r.hasVideo {
		// TODO: the webm writer should NOT have uneeded tracks if they are not necessary.
		// Review this - prlanzarin
		if width == 0 || height == 0 {
			width = 640
			height = 480
			log.WithField("session", r.ctx.Value("session")).
				Debug("Using default video dimensions for audio-only initialization")
		}

		tracks = append(tracks, trackConfig{codec: r.videoCodec, width: width, height: height, codecPrivate: codecPrivate})
	}

	// Audio track is optional
	if r.hasAudio {
		tracks = append(tracks, trackConfig{codec: CodecOpus})
	}

	log.WithField("file", r.file).WithField("fileMode", r.fileMode).Debug("Opening file for writing")
	writers, err := openContainer(r.file, r.fileMode, r.format, tracks, r.sorterPacketQueueSize)

	if err != nil {
		// TODO review - panic is not the best choice here.
		panic(err)
	}

	log.WithField("session", r.ctx.Value("session")).
		Infof("%s writers started with video=%t, audio=%t : %s", r.format, r.hasVideo, r.hasAudio, r.file)

	writerIndex := 0

	if r.hasVideo {
		r.videoWriter = writers[writerIndex]
		writerIndex++
	}

	if r.hasAudio && writerIndex < len(writers) {
		r.audioWriter = writers[writerIndex]
	}

	r.started = true

	if r.writeIVFCopy && r.hasVideo && r.videoCodec != CodecVP8 {
		log.WithField("session", r.ctx.Value("session")).
			Infof("No IVF copy: it holds VP8 only, not %s", r.videoCodec.MimeType)
	} else if r.writeIVFCopy && r.hasVideo {
		if err := r.startIVFWriter(); err != nil {
			log.WithField("session", r.ctx.Value("session")).
				Warnf("Error starting RTP ivf: %v", err)
		}
	}
}

//  ----- Stats tracking methods -----

func (r *WebmRecorder) trackRTPDiscontinuity(stats *types.BaseTrackStats, gap uint16) {
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

func (r *WebmRecorder) trackFrameStats(stats *types.RecorderTrackStats, size int, isKeyFrame bool, duration time.Duration) {
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

func (r *WebmRecorder) trackSampleStats(stats *types.BaseTrackStats, duration time.Duration) {
	// Duration is in ns, convert to ms
	durationMs := duration / time.Millisecond

	if stats == nil {
		return
	}

	stats.TotalSamples++
	stats.SampleDurationAcc += durationMs

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
