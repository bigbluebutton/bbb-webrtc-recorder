package recorder

import (
	"fmt"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	log "github.com/sirupsen/logrus"
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

func (r *WebmRecorder) pushVP8Builtin(packet *rtp.Packet, at time.Time) {
	if !r.hasVideo {
		return
	}

	r.m.Lock()
	defer r.m.Unlock()

	if len(packet.Payload) == 0 || r.closed {
		return
	}

	r.initVideoStats()
	r.span.arrived(packet.Timestamp, at)
	isKeyFrame := IsVP8KeyFrame(packet)

	if isKeyFrame {
		r.currKeyFrame = packet
		r.lastKeyFrameTime = time.Now()
	}

	if r.videoSeqTracker.expectedNextSeq > 0 && packet.SequenceNumber != r.videoSeqTracker.expectedNextSeq {
		gap := calculateSequenceGap(packet.SequenceNumber, r.videoSeqTracker.expectedNextSeq)
		r.trackRTPDiscontinuity(&r.stats.Video.BaseTrackStats, gap)

		log.WithField("session", r.ctx.Value("session")).
			WithField("expected_seq", r.videoSeqTracker.expectedNextSeq).
			WithField("got_seq", packet.SequenceNumber).
			WithField("gap", gap).
			Debug("Video sequence discontinuity detected")
	}

	r.setExpectedNextSeq(packet.SequenceNumber, "video")
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
			pts := int64(r.videoTimestamp / time.Millisecond)
			log.WithField("session", r.ctx.Value("session")).
				Tracef("Writing VP8 frame: ts=%d, size=%d, KF=%v", ts, len(sample.Data), isKf)

			if _, err := r.videoWriter.Write(isKf, pts, sample.Data); err != nil {
				log.WithField("session", r.ctx.Value("session")).
					Errorf("Error writing video frame: %v", err)
				r.hasKeyFrame = false
				r.RequestKeyframe()
			} else {
				r.stats.Video.WrittenSamples++
				r.span.wrote(ts, time.Duration(pts)*time.Millisecond, duration, videoSpan)
				log.WithField("session", r.ctx.Value("session")).
					Tracef("VP8 frame written: ts=%d, size=%d, KF=%v", ts, len(sample.Data), isKf)
			}
		}

		if r.writeIVFCopy {
			r.pushToIVFWriter(sample.Data, packet)
		}
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

	return isVP8KeyFrameStart(&vp8Packet)
}

// isVP8KeyFrameStart reports whether vp8Packet starts a key frame. Its
// payload descriptor can fill the RTP payload, leaving no frame header.
func isVP8KeyFrameStart(vp8Packet *codecs.VP8Packet) bool {
	return vp8Packet.S != 0 && vp8Packet.PID == 0 && len(vp8Packet.Payload) > 0 && vp8Packet.Payload[0]&0x1 == 0
}

func GetVP8KFDimension(packet *rtp.Packet) (int, int) {
	if len(packet.Payload) == 0 {
		return 0, 0
	}

	vp8Packet := codecs.VP8Packet{}

	if _, err := vp8Packet.Unmarshal(packet.Payload); err != nil {
		return 0, 0
	}

	if vp8Packet.S != 0 || vp8Packet.PID != 0 || len(vp8Packet.Payload) < 10 {
		return 0, 0
	}

	raw := uint(vp8Packet.Payload[6]) | uint(vp8Packet.Payload[7])<<8 | uint(vp8Packet.Payload[8])<<16 | uint(vp8Packet.Payload[9])<<24
	width := int(raw & 0x3FFF)
	height := int((raw >> 16) & 0x3FFF)

	return width, height
}

func (r *WebmRecorder) pushVP8Custom(p *rtp.Packet, at time.Time) {
	if !r.hasVideo {
		return
	}

	r.m.Lock()
	defer r.m.Unlock()

	if len(p.Payload) == 0 || r.closed {
		return
	}

	r.initVideoStats()
	r.span.arrived(p.Timestamp, at)

	if r.videoSeqTracker.expectedNextSeq > 0 && p.SequenceNumber != r.videoSeqTracker.expectedNextSeq {
		gap := calculateSequenceGap(p.SequenceNumber, r.videoSeqTracker.expectedNextSeq)
		r.trackRTPDiscontinuity(&r.stats.Video.BaseTrackStats, gap)

		if !r.skipSignaled {
			log.WithField("session", r.ctx.Value("session")).
				WithField("expected_seq", r.videoSeqTracker.expectedNextSeq).
				WithField("got_seq", p.SequenceNumber).
				WithField("gap", gap).
				Debug("Video sequence discontinuity detected")
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

	r.setExpectedNextSeq(p.SequenceNumber, "video")
	r.lastProcessedSeq = p.SequenceNumber

	vp8Packet := codecs.VP8Packet{}

	if _, err := vp8Packet.Unmarshal(p.Payload); err != nil {
		log.WithField("session", r.ctx.Value("session")).
			Debugf("Failed to unmarshal VP8 packet: seq=%d, err=%v", p.SequenceNumber, err)
		r.hasKeyFrame = false
		r.RequestKeyframe()
		return
	}

	isKeyFrame := isVP8KeyFrameStart(&vp8Packet)
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
		if len(r.currentFrame) > 0 {
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

		if isKeyFrame {
			r.lastKeyFrameTime = r.frameStartTime
		}

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
					if !r.keyframeDue(p.SSRC) {
						r.RequestKeyframe()
					}
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
			var sSeq, eSeq, pktCount, stime, pictureID, timestamp = 0, 0, 0, 0, 0, 0

			r.stats.Video.WrittenSamples++
			r.span.wrote(p.Timestamp, time.Duration(newPts)*time.Millisecond, duration, videoSpan)

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

			if err := r.ivfWriter.UpdateDimensions(width, height); err != nil {
				log.WithField("session", r.ctx.Value("session")).
					Warnf("Failed to update the IVF copy's dimensions: %v", err)
			}
		}

		err := r.ivfWriter.WriteFrame(frame, p.Timestamp, isKeyFrame)

		if err != nil {
			log.WithField("session", r.ctx.Value("session")).
				Warnf("Failed to write frame to IVF copy: %v", err)
		}
	}
}

func (r *WebmRecorder) trackPicIDDiscontinuity(stats *types.RecorderTrackStats, gap uint16) {
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
