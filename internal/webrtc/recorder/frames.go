package recorder

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pion/rtp"
	log "github.com/sirupsen/logrus"
)

// ArrivedPacket is an RTP packet and the time it arrived.
type ArrivedPacket struct {
	*rtp.Packet
	At time.Time
}

// A VideoSample is the packets of whole frames that a jitter buffer releases
// together, in sequence order.
type VideoSample struct {
	Packets []ArrivedPacket
	// Follows reports whether no packet was lost between the previous sample
	// and this one.
	Follows bool
}

// videoFrame is one coded picture as the container stores it.
type videoFrame struct {
	// lost marks frames dropped in this place: decoding resumes at the
	// next key frame. A lost frame holds nothing else.
	lost      bool
	data      []byte
	timestamp uint32
	// keyFrame reports whether decoding can start at the frame.
	keyFrame bool
	// Set on key frames: the picture size, and the codec configuration the
	// container's track entry needs.
	width, height int
	codecPrivate  []byte
}

// errUnusableKeyFrame is a key frame that cannot start decoding. Its
// parameter sets are missing, or its picture is as small as LiveKit's blank
// frames at a downtrack's close.
var errUnusableKeyFrame = errors.New("key frame that cannot start decoding")

// Key frames that cannot start decoding: the picture is too large, its size
// cannot be read, or avcC cannot hold the parameter sets. Unlike LiveKit's
// blank frames, they are not routine.
var (
	errOversizedKeyFrame  = errors.New("invalid dimensions")
	errUnreadableKeyFrame = errors.New("unknown dimensions")
	errAvcCKeyFrame       = errors.New("parameter sets avcC cannot hold")
)

// errCutFrame is a frame cut before it is whole, with no packet lost: the
// next frame or the end of the sample comes first. It is routine: LiveKit
// cuts frames this way at a simulcast layer switch, and when it pauses a
// downtrack.
var errCutFrame = errors.New("cut short")

// routineFrameError reports whether err is a loss LiveKit causes in normal
// operation, which PushVideoSample logs at Debug.
func routineFrameError(err error) bool {
	return errors.Is(err, errUnusableKeyFrame) || errors.Is(err, errCutFrame)
}

// refusedKeyFrame returns the kind of refused key frame err wraps, or nil.
func refusedKeyFrame(err error) error {
	for _, kind := range []error{errOversizedKeyFrame, errUnreadableKeyFrame, errAvcCKeyFrame} {
		if errors.Is(err, kind) {
			return kind
		}
	}

	return nil
}

// FormatFor returns Matroska if the recorder writes one of mimeTypes and
// WebM cannot hold it. A codec it does not write fails in either format.
func FormatFor(mimeTypes []string) Format {
	for _, m := range mimeTypes {
		if c, ok := CodecFor(m); ok && !c.WebM {
			return FormatMKV
		}
	}

	return FormatWebM
}

// formatOf splits a recording's file name into its format and the name
// without extension. A name with no format extension keeps it whole.
func formatOf(file string) (Format, string) {
	for _, f := range []Format{FormatWebM, FormatMKV} {
		if strings.EqualFold(filepath.Ext(file), f.Extension()) {
			return f, strings.TrimSuffix(file, filepath.Ext(file))
		}
	}

	return FormatWebM, file
}

func (r *WebmRecorder) Format() Format {
	r.m.Lock()
	defer r.m.Unlock()

	return r.format
}

// SetFormat sets the recording's container, and the extension of its file
// name. It fails once the file is open in another format, or when f cannot
// hold the video codec already set.
func (r *WebmRecorder) SetFormat(f Format) error {
	r.m.Lock()
	defer r.m.Unlock()

	switch {
	case f == r.format:
		return nil
	case r.started:
		return fmt.Errorf("the recording is already open as %s", r.format)
	case f == FormatWebM && !r.videoCodec.WebM:
		return fmt.Errorf("a WebM file cannot hold %s", r.videoCodec.MimeType)
	}

	r.format = f
	if r.file != os.DevNull {
		r.fileMu.Lock()
		r.file = r.base + f.Extension()
		r.fileMu.Unlock()
	}

	return nil
}

// SetVideoCodec sets the codec of the video track, VP8 by default. It fails
// for a codec the recording's format cannot hold, and once the file is open
// with another codec.
func (r *WebmRecorder) SetVideoCodec(c *Codec) error {
	r.m.Lock()
	defer r.m.Unlock()

	switch {
	case c == r.videoCodec:
		return nil
	case !c.Video:
		return fmt.Errorf("%s is not a video codec", c.MimeType)
	case r.started:
		return fmt.Errorf("the recording already holds %s", r.videoCodec.MimeType)
	case r.format == FormatWebM && !c.WebM:
		return fmt.Errorf("a WebM file cannot hold %s", c.MimeType)
	}

	r.videoCodec = c

	return nil
}

// Codecs returns the MIME types of the recording's tracks.
func (r *WebmRecorder) Codecs() []string {
	r.m.Lock()
	defer r.m.Unlock()

	var mimeTypes []string
	if r.hasVideo {
		mimeTypes = append(mimeTypes, r.videoCodec.MimeType)
	}
	if r.hasAudio {
		mimeTypes = append(mimeTypes, CodecOpus.MimeType)
	}

	return mimeTypes
}

// PushVideoSample pushes the frames in s. A codec without frames of its own,
// such as VP8, takes the packets one at a time.
func (r *WebmRecorder) PushVideoSample(s VideoSample) {
	if !r.hasVideo || len(s.Packets) == 0 {
		return
	}

	// The codec is read and used under one lock: SetVideoCodec can change it
	// until the file opens.
	r.m.Lock()
	if r.videoCodec.frames == nil {
		r.m.Unlock()

		for _, p := range s.Packets {
			r.PushVideoAt(p.Packet, p.At)
		}

		return
	}
	defer r.m.Unlock()

	if r.closed {
		return
	}

	r.initVideoStats()

	packets := make([]*rtp.Packet, 0, len(s.Packets))
	for _, p := range s.Packets {
		r.span.arrived(p.Timestamp, p.At)
		packets = append(packets, p.Packet)
	}

	first, last := packets[0].SequenceNumber, packets[len(packets)-1].SequenceNumber
	if r.videoSeqTracker.expectedNextSeq > 0 && first != r.videoSeqTracker.expectedNextSeq {
		r.trackRTPDiscontinuity(&r.stats.Video.BaseTrackStats, calculateSequenceGap(first, r.videoSeqTracker.expectedNextSeq))
	}
	r.setExpectedNextSeq(last, "video")

	ssrc := packets[0].SSRC
	if !s.Follows {
		r.hasKeyFrame = false
	}

	frames, err := r.videoCodec.frames(packets)
	lost := false
	for _, f := range frames {
		if f.lost {
			r.hasKeyFrame, lost = false, true

			continue
		}

		r.writeVideoFrame(f, ssrc, arrivalOf(s.Packets, f.timestamp))
	}

	if err != nil {
		logger := log.WithField("session", r.ctx.Value("session")).
			WithField("seq", fmt.Sprintf("%d-%d", first, last))
		switch refused := refusedKeyFrame(err); {
		case refused != nil:
			logger.Logf(r.logLevel(refused), "Discarding invalid %s frame: %v", r.videoCodec.MimeType, err)
		case routineFrameError(err):
			logger.Debugf("Discarding %s frame: %v", r.videoCodec.MimeType, err)
		default:
			logger.Logf(r.logLevel("malformed"), "Discarding malformed %s frame: %v", r.videoCodec.MimeType, err)
		}
	}

	// No request when a key frame followed the frames lost in this sample.
	if lost && !r.hasKeyFrame {
		r.requestKeyframeUnlessDue(ssrc)
	}
}

// logLevel returns Warn the first time the recording logs a message of
// kind, and Debug after. A publisher that sends one bad frame often sends
// one with every frame.
func (r *WebmRecorder) logLevel(kind any) log.Level {
	if slices.Contains(r.warned, kind) {
		return log.DebugLevel
	}
	r.warned = append(r.warned, kind)

	return log.WarnLevel
}

func (r *WebmRecorder) requestKeyframeUnlessDue(ssrc uint32) {
	if !r.keyframeDue(ssrc) {
		r.RequestKeyframe()
	}
}

// Locked. writeVideoFrame writes f, unless f is over maxFrameSize, the
// writer waits for a key frame, or f is a delta frame whose timestamp goes
// back. at is when f arrived.
func (r *WebmRecorder) writeVideoFrame(f videoFrame, ssrc uint32, at time.Time) {
	// Signed, so that a timestamp going back gives no interval rather than
	// one of 13 hours.
	var interval time.Duration
	if d := int32(f.timestamp - r.lastFrameTS); r.framesSeen && d > 0 {
		interval = r.ticks(uint32(d))
	}
	r.framesSeen, r.lastFrameTS = true, f.timestamp
	r.trackFrameStats(r.stats.Video, len(f.data), f.keyFrame, interval)

	if len(f.data) > r.maxFrameSize {
		log.WithField("session", r.ctx.Value("session")).
			Logf(r.logLevel("too large"), "Discarding %s frame of %d bytes: the limit is %d",
				r.videoCodec.MimeType, len(f.data), r.maxFrameSize)
		r.hasKeyFrame = false
		r.requestKeyframeUnlessDue(ssrc)

		return
	}

	if !r.hasKeyFrame {
		if !f.keyFrame {
			r.requestKeyframeUnlessDue(ssrc)

			return
		}

		if !r.seenKeyFrame {
			log.WithField("session", r.ctx.Value("session")).
				Infof("First %s key frame: timestamp=%d, %dx%d", r.videoCodec.MimeType, f.timestamp, f.width, f.height)

			r.seenKeyFrame = true
			r.packetTimestamp = f.timestamp
		}

		r.hasKeyFrame = true
	}

	// A B-frame, which WebRTC encoders do not send, or a clock step back
	// would get a PTS hours off. A forward jump of 2^31 to 2^32 ticks reads
	// the same, since int32 cannot tell it from a step back. The frames that
	// may reference a skipped one are skipped too, until a key frame. A key
	// frame that is behind starts a new timestamp base.
	if delta := int32(f.timestamp - r.packetTimestamp); delta < 0 {
		logger := log.WithField("session", r.ctx.Value("session")).
			WithField("timestamp", f.timestamp).
			WithField("last", r.packetTimestamp)
		if !f.keyFrame {
			logger.Log(r.logLevel("goes back"), "Skipping video frame whose timestamp goes back")
			r.hasKeyFrame = false
			r.requestKeyframeUnlessDue(ssrc)

			return
		}

		logger.Log(r.logLevel("new base"), "Video timestamps restart at a key frame")
		// The last frame written can arrive with this key frame, or even
		// after it when that frame was resent. The key frame is still placed
		// at least 1 ms after the last frame, so that the two do not share a
		// PTS.
		r.videoTicks += max(r.ticksBetween(r.lastWrittenAt, at), uint64(r.videoCodec.ClockRate)/1000)
		r.packetTimestamp = f.timestamp
	}

	// From the ticks since the first key frame: adding up rounded frame
	// durations would drift.
	ticks := r.videoTicks + uint64(f.timestamp-r.packetTimestamp)
	videoTimestamp := r.ticksToTime(ticks)
	duration := videoTimestamp - r.videoTimestamp
	pts := int64(videoTimestamp / time.Millisecond)

	if r.videoWriter == nil {
		r.openWriter(f.width, f.height, f.codecPrivate)
	}

	if r.pts > 0 && pts == r.pts {
		log.WithField("session", r.ctx.Value("session")).
			Logf(r.logLevel("duplicate"), "Duplicate frame detected: pts=%d, ts=%d", pts, f.timestamp)
	}

	if _, err := r.videoWriter.Write(f.keyFrame, pts, f.data); err != nil {
		log.WithField("session", r.ctx.Value("session")).
			Errorf("Error writing video frame: %v", err)
		r.hasKeyFrame = false
		r.RequestKeyframe()

		return
	}

	r.videoTicks = ticks
	r.lastWrittenAt = at
	r.videoTimestamp = videoTimestamp
	r.packetTimestamp = f.timestamp
	r.pts = pts
	r.hasValidVideo = true
	r.stats.Video.WrittenSamples++
	r.span.wrote(f.timestamp, time.Duration(pts)*time.Millisecond, duration, videoSpan)

	if f.keyFrame {
		r.lastKeyFrameTime = time.Now()
	}
}

// ticks converts an RTP timestamp difference of the video codec to time.
func (r *WebmRecorder) ticks(d uint32) time.Duration {
	return r.ticksToTime(uint64(d))
}

// ticksBetween converts the time between two arrivals to RTP ticks, 0 when
// one is unknown.
func (r *WebmRecorder) ticksBetween(from, to time.Time) uint64 {
	if from.IsZero() || !to.After(from) {
		return 0
	}

	return uint64(to.Sub(from)/time.Microsecond) * uint64(r.videoCodec.ClockRate) / 1e6
}

// arrivalOf returns when the earliest packet of the frame at RTP timestamp
// ts arrived.
func arrivalOf(packets []ArrivedPacket, ts uint32) time.Time {
	var at time.Time
	for _, p := range packets {
		if p.Timestamp == ts && (at.IsZero() || p.At.Before(at)) {
			at = p.At
		}
	}

	return at
}

func (r *WebmRecorder) ticksToTime(ticks uint64) time.Duration {
	rate := uint64(r.videoCodec.ClockRate)

	return time.Duration(ticks/rate)*time.Second + time.Duration(ticks%rate)*time.Second/time.Duration(rate)
}
