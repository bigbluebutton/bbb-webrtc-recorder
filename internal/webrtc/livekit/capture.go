package livekit

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
)

const (
	// The eof reasons of a trace that misses records: cut at its byte cap,
	// or with records dropped while its writer fell behind.
	truncatedReason = "truncated"
	droppedReason   = "dropped"

	// captureQueueLen bounds the records waiting for a capture's writer: a
	// full queue drops records rather than hold up the recording.
	captureQueueLen = 1024
)

// trackCapture writes one track's reads as an internal/corpus read trace:
// its packets, read timeouts and keyframe requests. observe and
// keyframeRequest only queue records for its writer goroutine, so they never
// wait on the file. Once the trace ends, by close, the byte cap or a write
// error, every later record is ignored.
type trackCapture struct {
	ssrc     uint32
	full     bool
	vp8      bool
	maxBytes int64
	now      func() int64
	logger   *log.Entry
	out      io.Closer
	tw       *corpus.Writer
	records  chan corpus.Record
	done     chan struct{} // closed once the writer goroutine is gone

	mu      sync.Mutex
	closed  bool // records is closed
	stopped bool // the trace has ended before close
	dropped int
	closeAt int64
}

// newTrackCapture starts a trace with header h on out, which it closes when
// the trace ends, and the writer goroutine that close stops. now gives the
// read time of each record.
func newTrackCapture(out io.WriteCloser, h corpus.Header, full bool, maxBytes int64,
	now func() int64, logger *log.Entry) (*trackCapture, error) {
	tw, err := corpus.NewWriter(out, h)
	if err != nil {
		return nil, err
	}

	c := &trackCapture{
		ssrc:     h.SSRC,
		full:     full,
		vp8:      h.Codec == corpus.CodecVP8,
		maxBytes: maxBytes,
		now:      now,
		logger:   logger,
		out:      out,
		tw:       tw,
		records:  make(chan corpus.Record, captureQueueLen),
		done:     make(chan struct{}),
	}
	go c.run()

	return c, nil
}

// observe records what a ReadRTP returned: a packet, or a timeout. Other
// errors are not recorded; the terminal ones end the read loop, which
// closes the capture.
func (c *trackCapture) observe(p *rtp.Packet, attrs interceptor.Attributes, err error) {
	at := c.now()

	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			c.send(corpus.Record{T: corpus.RecordTimeout, AtUs: at})
		}

		return
	}

	if p == nil {
		return
	}

	c.send(corpus.Record{
		T:    corpus.RecordRTP,
		AtUs: at,
		Seq:  p.SequenceNumber,
		TS:   p.Timestamp,
		M:    p.Marker,
		PT:   p.PayloadType,
		RTX:  attrs.Get(webrtc.AttributeRtxSsrc) != nil,
		Pad:  p.Padding,
		PLen: len(p.Payload),
		Pay:  slices.Clone(c.payloadPrefix(p.Payload)),
	})
}

func (c *trackCapture) keyframeRequest(reason string) {
	c.send(corpus.Record{T: corpus.RecordPLI, AtUs: c.now(), Reason: reason})
}

// send queues r for the writer, or counts it dropped if the queue is full.
func (c *trackCapture) send(r corpus.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed || c.stopped {
		return
	}

	select {
	case c.records <- r:
	default:
		c.dropped++
	}
}

func (c *trackCapture) payloadPrefix(p []byte) []byte {
	switch {
	case c.full:
		return p
	case c.vp8:
		return p[:vp8HeaderLen(p)]
	case len(p) <= 3:
		return p // Opus DTX frames, which carry no media
	default:
		return p[:1] // the Opus TOC byte
	}
}

// vp8HeaderLen is the length of p's VP8 payload descriptor (RFC 7741 4.2)
// plus, on the first packet of a frame, its frame header (RFC 6386 9.1):
// the 3-byte frame tag, and on a key frame the start code and dimensions
// too. What follows is media. It is capped at len(p).
func vp8HeaderLen(p []byte) int {
	if len(p) == 0 {
		return 0
	}

	n := 1
	if p[0]&0x80 != 0 { // X: an extension byte follows
		if len(p) < 2 {
			return len(p)
		}

		ext := p[1]
		n = 2

		if ext&0x80 != 0 { // I: a picture ID, 15 bits if its first bit is set
			if len(p) <= n {
				return len(p)
			}

			n++
			if p[2]&0x80 != 0 {
				n++
			}
		}

		if ext&0x40 != 0 { // L: TL0PICIDX
			n++
		}

		if ext&0x30 != 0 { // T or K: one TID/Y/KEYIDX byte
			n++
		}
	}

	// S set and partition index 0: the frame starts here.
	if p[0]&0x17 == 0x10 && n < len(p) {
		if p[n]&0x01 == 0 { // the frame tag's inverse key frame bit
			n += 10
		} else {
			n += 3
		}
	}

	return min(n, len(p))
}

// run writes the queued records until close, then ends the trace.
func (c *trackCapture) run() {
	defer close(c.done)

	ended := false
	var last int64

	for r := range c.records {
		if ended {
			continue
		}

		// Senders stamp a record before queueing it, so two of them can queue
		// out of time order: a record never takes a time before its predecessor's.
		r.AtUs = max(r.AtUs, last)
		last = r.AtUs

		if err := c.tw.WriteRecord(r); err != nil {
			ended = true
			c.stop()
			_ = c.tw.Close()
			_ = c.out.Close()
			c.logger.Warnf("Trace capture stopped on a write error: %v", err)

			continue
		}

		// Written lags the records by up to a flush interval, so a trace can
		// pass the cap by that much.
		if c.maxBytes > 0 && c.tw.Written() > c.maxBytes {
			ended = true
			if err := c.finish(truncatedReason, last); err != nil {
				c.logger.Warnf("Trace capture truncated at its %d-byte cap, and failed to close: %v", c.maxBytes, err)
			} else {
				c.logger.Warnf("Trace capture truncated at its %d-byte cap", c.maxBytes)
			}
		}
	}

	c.mu.Lock()
	closeAt, dropped := c.closeAt, c.dropped
	c.mu.Unlock()

	if !ended {
		if err := c.finish("", max(closeAt, last)); err != nil {
			c.logger.Warnf("Failed to close trace capture: %v", err)
		}
	}

	if dropped > 0 {
		c.logger.Warnf("Trace capture dropped %d records, its writer falling behind", dropped)
	}
}

// stop makes later records no-ops.
func (c *trackCapture) stop() {
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
}

// finish ends the trace with an eof record at at, whose reason is
// droppedReason once a record was dropped.
func (c *trackCapture) finish(reason string, at int64) error {
	c.mu.Lock()
	c.stopped = true
	if c.dropped > 0 {
		reason = droppedReason
	}
	c.mu.Unlock()

	err := c.tw.WriteRecord(corpus.Record{T: corpus.RecordEOF, AtUs: at, Reason: reason})

	return errors.Join(err, c.tw.Close(), c.out.Close())
}

// close ends the trace with an eof record, unless it has ended already, and
// returns once the writer goroutine has written what was queued and exited.
func (c *trackCapture) close() {
	at := c.now()

	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.closeAt = at
		close(c.records)
	}
	c.mu.Unlock()

	<-c.done
}

// traceCapturePath is <base>.<trackID>.trace.ndjson.gz next to the recording.
func traceCapturePath(recordingPath, trackID string) string {
	return trackFilePath(recordingPath, trackID, ".trace.ndjson.gz")
}

// sinceStart is the read-time clock of every trace of the session, so
// that its tracks' traces share one timeline.
func (w *LiveKitWebRTC) sinceStart() int64 {
	return time.Since(w.startTs).Microseconds()
}

// openTraceCapture starts info's trace for readTrack and the keyframe
// request queue to write to. A failure only leaves the track uncaptured.
func (w *LiveKitWebRTC) openTraceCapture(info trackInfo, clockRate uint32) {
	logger := log.WithField("session", w.ctx.Value("session")).WithField("trackID", info.id)

	recordingPath := w.rec.GetFilePath()
	if recordingPath == os.DevNull {
		logger.Infof("Trace capture skipped for track %s: the recording goes to %s", info.id, os.DevNull)

		return
	}

	payload := w.cfg.TraceCapture.Payload
	if payload != config.TraceCapturePayloadHeader && payload != config.TraceCapturePayloadFull {
		logger.Warnf("Unknown trace capture payload %q, capturing %q", payload, config.TraceCapturePayloadHeader)
		payload = config.TraceCapturePayloadHeader
	}

	maxBytes := w.cfg.TraceCapture.MaxBytesPerTrack
	if maxBytes < 0 {
		logger.Warnf("Negative trace capture maxBytesPerTrack %d, capping at %d", maxBytes, config.DefaultTraceCaptureMaxBytesPerTrack)
		maxBytes = config.DefaultTraceCaptureMaxBytesPerTrack
	}

	h := corpus.Header{
		V:           corpus.Version,
		Kind:        corpus.Kind(info.kind),
		ClockRate:   clockRate,
		SSRC:        info.ssrc,
		TrackID:     info.id,
		EpochUnixUs: w.startTs.UnixMicro(),
		Provenance:  corpus.ProvenanceCapture,
		Generator:   fmt.Sprintf("bbb-webrtc-recorder %s traceCapture/%s", internal.AppVersion, payload),
	}

	switch info.mime {
	case MimeTypeVP8:
		h.Codec = corpus.CodecVP8
	case MimeTypeOpus:
		h.Codec = corpus.CodecOpus
	}

	// A track subscribed again reuses the path: its earlier capture must
	// stop writing before the file is truncated.
	w.capturesMu.Lock()
	prev := w.traceCaptures[info.id]
	delete(w.traceCaptures, info.id)
	w.capturesMu.Unlock()

	if prev != nil {
		prev.close()
	}

	path := traceCapturePath(recordingPath, info.id)
	logger = logger.WithField("path", path)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		logger.Warnf("Failed to create trace capture for track %s: %v", info.id, err)

		return
	}

	c, err := newTrackCapture(f, h, payload == config.TraceCapturePayloadFull,
		maxBytes, w.sinceStart, logger)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		logger.Warnf("Failed to start trace capture for track %s: %v", info.id, err)

		return
	}

	w.capturesMu.Lock()
	// As for the rtp dump, a subscription can land after Close.
	if w.traceCapturesClosed {
		w.capturesMu.Unlock()
		c.close()
		_ = os.Remove(path)

		return
	}

	if w.traceCaptures == nil {
		w.traceCaptures = make(map[string]*trackCapture)
	}

	// A concurrent subscription of the same track can have stored its own.
	prev = w.traceCaptures[info.id]
	w.traceCaptures[info.id] = c
	w.capturesMu.Unlock()

	if prev != nil {
		prev.close()
	}

	logger.Infof("Trace capture enabled for track %s to %s", info.id, path)
}

// captureKeyframeRequest records a queued request in the trace of the
// track with ssrc, if it is captured. Like replay, it records requests
// made: one still queued when Close stops the processor is never sent.
func (w *LiveKitWebRTC) captureKeyframeRequest(ssrc uint32, reason string) {
	var target *trackCapture

	w.capturesMu.Lock()
	for _, c := range w.traceCaptures {
		if c.ssrc == ssrc {
			target = c

			break
		}
	}
	w.capturesMu.Unlock()

	if target != nil {
		target.keyframeRequest(reason)
	}
}

// closeTraceCaptures ends every trace and stops new ones from starting.
func (w *LiveKitWebRTC) closeTraceCaptures() {
	w.capturesMu.Lock()
	captures := w.traceCaptures
	w.traceCaptures = nil
	w.traceCapturesClosed = true
	w.capturesMu.Unlock()

	for _, c := range captures {
		c.close()
	}
}
