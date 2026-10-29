//go:build e2e && linux

package e2e

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/stretchr/testify/require"
)

// Publisher is a LiveKit participant that publishes generated media.
type Publisher struct {
	Identity string
	Room     *lksdk.Room

	t      *testing.T
	lk     *LiveKit
	room   string
	dir    string
	mu     sync.Mutex
	tracks []*PubTrack
	closed bool
}

// NewPublisher joins room as identity; its tracks' artifacts go to dir.
func NewPublisher(t *testing.T, lk *LiveKit, dir, room, identity string) *Publisher {
	t.Helper()

	r, err := lksdk.ConnectToRoomWithToken(lk.URL, lk.Token(t, room, identity, nil),
		&lksdk.RoomCallback{}, lksdk.WithAutoSubscribe(false))
	require.NoError(t, err, "publisher %s: connect", identity)

	p := &Publisher{Identity: identity, Room: r, t: t, lk: lk, room: room, dir: dir}
	t.Cleanup(p.Close)

	return p
}

// TrackOptions configures a published track; zero values select the defaults noted.
type TrackOptions struct {
	Name string
	// Source defaults to CAMERA for video and MICROPHONE for audio.
	Source livekit.TrackSource
	// Schedule is the sent schedule (corpus.GenerateVP8/GenerateOpus): each
	// record's packet is written at its atUs from the track's start. It
	// loops until the track stops, continuing sequence numbers, timestamps
	// and VP8 picture IDs.
	Schedule *corpus.Trace
	// KeyframeDelay is the encoder's PLI response time: the first frame due
	// at least this long after a PLI goes out as a key frame. PLIs while one
	// is pending coalesce, as encoders do.
	KeyframeDelay time.Duration
	// KeyframeSpread paces a PLI's key frame packets evenly over this span,
	// FIFO: frames due meanwhile queue behind it. 0 keeps the schedule's own
	// packet offsets.
	KeyframeSpread time.Duration
	// Drop withholds a packet when it returns true. Its sequence number is
	// still used and it never reaches the NACK responder, so the loss is
	// unrepaired.
	Drop func(SentPacket) bool
	// Width and Height are the signalled video dimensions (default 640x480).
	Width, Height int
	// TagFrames writes each frame's index into its first packet, after the
	// codec's header, for TaggedFrame to read from the recording.
	TagFrames bool
}

// SentPacket is one packet the publisher wrote, or withheld.
type SentPacket struct {
	At      time.Duration // since the track started sending
	Due     time.Duration // when the pacing scheduled it; At is later by the wakeup delay
	Seq     uint16
	TS      uint32
	Frame   int  // frame index since the track started
	Index   int  // the packet's index in its frame
	Key     bool // a key frame's packet
	PLIKey  bool // of a key frame sent for a PLI
	Marker  bool
	PLen    int
	Dropped bool
}

// PubTrack is a published track and its send loop.
type PubTrack struct {
	SID   string
	Kind  corpus.Kind
	Local *lksdk.LocalTrack
	Pub   *lksdk.LocalTrackPublication

	opts     TrackOptions
	frames   []schedFrame
	keyFrame []corpus.Record
	cancel   context.CancelFunc
	done     chan struct{}
	start    time.Time

	mu         sync.Mutex
	plis       []time.Duration
	nacks      int
	pendingDue time.Duration // when the pending PLI's key frame is due
	pending    bool
	paused     bool
	sent       []SentPacket
}

type schedFrame struct {
	atUs int64
	ts   uint32
	recs []corpus.Record
	key  bool
}

var (
	vp8Capability  = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}
	opusCapability = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
)

// PublishVP8 publishes a VP8 track sending o.Schedule.
func (p *Publisher) PublishVP8(o TrackOptions) *PubTrack {
	if o.Source == livekit.TrackSource_UNKNOWN {
		o.Source = livekit.TrackSource_CAMERA
	}

	o.Width = withDefault(o.Width, 640)
	o.Height = withDefault(o.Height, 480)

	return p.publish(vp8Capability, corpus.KindVideo, o)
}

// PublishOpus publishes an Opus track sending o.Schedule.
func (p *Publisher) PublishOpus(o TrackOptions) *PubTrack {
	if o.Source == livekit.TrackSource_UNKNOWN {
		o.Source = livekit.TrackSource_MICROPHONE
	}

	return p.publish(opusCapability, corpus.KindAudio, o)
}

func (p *Publisher) publish(c webrtc.RTPCodecCapability, kind corpus.Kind, o TrackOptions) *PubTrack {
	t := p.t
	t.Helper()

	tr := newPubTrack(t, c, kind, o)

	pub, err := p.Room.LocalParticipant.PublishTrack(tr.Local, &lksdk.TrackPublicationOptions{
		Name:        tr.opts.Name,
		Source:      tr.opts.Source,
		VideoWidth:  tr.opts.Width,
		VideoHeight: tr.opts.Height,
	})
	require.NoError(t, err, "publish %s", tr.opts.Name)

	tr.Pub = pub
	tr.SID = pub.SID()
	tr.startSending()

	p.mu.Lock()
	p.tracks = append(p.tracks, tr)
	p.mu.Unlock()

	t.Logf("publisher %s: %s track %s (%s) sending", p.Identity, kind, tr.SID, tr.opts.Name)

	return tr
}

// newPubTrack prepares a track's schedule and LocalTrack, not yet published.
func newPubTrack(t *testing.T, c webrtc.RTPCodecCapability, kind corpus.Kind, o TrackOptions) *PubTrack {
	t.Helper()

	require.NotNil(t, o.Schedule, "TrackOptions.Schedule")
	require.Equal(t, kind, o.Schedule.Header.Kind, "schedule kind")

	if o.Name == "" {
		o.Name = string(kind)
	}

	tr := &PubTrack{Kind: kind, opts: o, done: make(chan struct{})}
	tr.frames = scheduleFrames(o.Schedule.Records, kind == corpus.KindVideo)
	require.NotEmpty(t, tr.frames, "schedule %s has no packets", o.Name)

	if kind == corpus.KindVideo {
		tr.keyFrame = templateKeyFrame(t, tr.frames, o.Width, o.Height)
	}

	local, err := lksdk.NewLocalTrack(c, lksdk.WithRTCPHandler(tr.onRTCP))
	require.NoError(t, err)

	tr.Local = local

	return tr
}

func (tr *PubTrack) startSending() {
	ctx, cancel := context.WithCancel(context.Background())
	tr.cancel = cancel

	tr.mu.Lock()
	tr.start = time.Now()
	tr.mu.Unlock()

	go tr.run(ctx, tr.Start())
}

// Close stops every track, writes their artifacts and leaves the room.
func (p *Publisher) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}

	p.closed = true
	tracks := p.tracks
	p.mu.Unlock()

	for _, tr := range tracks {
		tr.Stop()
		tr.writeArtifacts(p.t, p.dir, p.Identity)
	}

	p.Room.Disconnect()
}

// WaitPublished waits until LiveKit lists every track of tracks as
// published. LiveKit announces a track once its media arrives, and a
// recorder joining before that finds no track to subscribe to; the SFU
// starts recording on that announcement (track_published).
func (p *Publisher) WaitPublished(ctx context.Context, tracks ...*PubTrack) error {
	sids := make([]string, 0, len(tracks))
	for _, tr := range tracks {
		sids = append(sids, tr.SID)
	}

	return waitTracksListed(ctx, p.lk, p.room, p.Identity, sids...)
}

// waitTracksListed waits until LiveKit lists every track of sids as
// published by identity in room.
func waitTracksListed(ctx context.Context, lk *LiveKit, room, identity string, sids ...string) error {
	rs := lksdk.NewRoomServiceClient(lk.HTTPURL(), lk.APIKey, lk.APISecret)

	for {
		resp, err := rs.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: room})
		if err == nil {
			listed := map[string]bool{}

			for _, pi := range resp.Participants {
				if pi.Identity == identity {
					for _, ti := range pi.Tracks {
						listed[ti.Sid] = true
					}
				}
			}

			missing := 0
			for _, sid := range sids {
				if !listed[sid] {
					missing++
				}
			}

			if missing == 0 {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("tracks not published: %w (last list error: %v)", ctx.Err(), err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Start is when the track began sending; SentPacket.At and PLIs count from it.
func (tr *PubTrack) Start() time.Time {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	return tr.start
}

// Stop ends the send loop and waits for it.
func (tr *PubTrack) Stop() {
	tr.cancel()
	<-tr.done
}

// Pause stalls the source: frames due until Resume are not sent and use no
// sequence numbers, so the receiver sees a timestamp jump, not a loss.
func (tr *PubTrack) Pause() {
	tr.mu.Lock()
	tr.paused = true
	tr.mu.Unlock()
}

// Resume ends a Pause from the next frame due.
func (tr *PubTrack) Resume() {
	tr.mu.Lock()
	tr.paused = false
	tr.mu.Unlock()
}

// PLIs returns the arrival time of each PLI or FIR received.
func (tr *PubTrack) PLIs() []time.Duration {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	return append([]time.Duration(nil), tr.plis...)
}

// NACKs returns the number of NACK packets received.
func (tr *PubTrack) NACKs() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	return tr.nacks
}

// Sent returns every packet written or withheld so far.
func (tr *PubTrack) Sent() []SentPacket {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	return append([]SentPacket(nil), tr.sent...)
}

// SentBetween counts the frames sent (fully or partly) and the key frames
// among them in [from, to) since the track's start.
func (tr *PubTrack) SentBetween(from, to time.Duration) (frames, keyFrames int) {
	last := -1

	for _, s := range tr.Sent() {
		if s.Dropped || s.At < from || s.At >= to || s.Frame == last {
			continue
		}

		last = s.Frame
		frames++

		if s.Key {
			keyFrames++
		}
	}

	return frames, keyFrames
}

// The RTCP handler runs on the track's RTCP reader goroutine: keep it short.
func (tr *PubTrack) onRTCP(pkt rtcp.Packet) {
	switch pkt.(type) {
	case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
		tr.mu.Lock()
		// A PLI can arrive as the track binds, before it starts sending.
		var at time.Duration
		if !tr.start.IsZero() {
			at = time.Since(tr.start)
		}

		tr.plis = append(tr.plis, at)

		if tr.Kind == corpus.KindVideo && !tr.pending {
			tr.pending = true
			tr.pendingDue = at + tr.opts.KeyframeDelay
		}
		tr.mu.Unlock()
	case *rtcp.TransportLayerNack:
		tr.mu.Lock()
		tr.nacks++
		tr.mu.Unlock()
	}
}

func (tr *PubTrack) run(ctx context.Context, start time.Time) {
	defer close(tr.done)

	frames := tr.frames
	first := frames[0]
	spanUs, spanTS := loopSpan(frames)

	seq := first.recs[0].Seq
	var picID uint16
	if tr.Kind == corpus.KindVideo {
		picID, _ = vp8PictureID(first.recs[0].Pay)
	}

	var prevSend time.Duration

	for loop := 0; ; loop++ {
		for i, f := range frames {
			frameAt := time.Duration(int64(loop)*spanUs+f.atUs-first.atUs) * time.Microsecond
			ts := f.ts + uint32(loop)*spanTS
			frame := loop*len(frames) + i

			if !sleepUntil(ctx, start.Add(frameAt)) {
				return
			}

			tr.mu.Lock()
			if tr.paused {
				tr.mu.Unlock()
				continue
			}

			pliKey := tr.pending && frameAt >= tr.pendingDue && !f.key
			if pliKey || f.key {
				tr.pending = false
			}
			tr.mu.Unlock()

			recs, offsets := f.recs, packetOffsets(f.recs, 0)
			if pliKey {
				recs, offsets = tr.keyFrame, packetOffsets(tr.keyFrame, tr.opts.KeyframeSpread)
			}

			for j, r := range recs {
				sendAt := max(frameAt+offsets[j], prevSend)
				if !sleepUntil(ctx, start.Add(sendAt)) {
					return
				}

				prevSend = sendAt

				pkt := r.Packet()
				pkt.SequenceNumber = seq
				pkt.Timestamp = ts
				pkt.Marker = r.M && (loop == 0 || tr.Kind == corpus.KindVideo)

				if tr.Kind == corpus.KindVideo {
					setVP8PictureID(pkt.Payload, picID)
				}

				if tr.opts.TagFrames && j == 0 {
					tagFrame(pkt.Payload, tr.Kind, f.key || pliKey, frame)
				}

				s := SentPacket{
					At:     time.Since(start),
					Due:    sendAt,
					Seq:    seq,
					TS:     ts,
					Frame:  frame,
					Index:  j,
					Key:    f.key || pliKey,
					PLIKey: pliKey,
					Marker: pkt.Marker,
					PLen:   len(pkt.Payload),
				}
				s.Dropped = tr.opts.Drop != nil && tr.opts.Drop(s)

				if !s.Dropped {
					// Fails only before the track is bound or after it is
					// closed; the packet is lost either way, as a real one would be.
					_ = tr.Local.WriteRTP(pkt, nil)
				}

				tr.mu.Lock()
				tr.sent = append(tr.sent, s)
				tr.mu.Unlock()

				seq++
			}

			picID = (picID + 1) & 0x7FFF
		}
	}
}

// writeArtifacts saves the sent schedule as a trace and a JSON summary of
// the track's PLIs, NACKs and withheld packets.
func (tr *PubTrack) writeArtifacts(t *testing.T, dir, identity string) {
	base := filepath.Join(dir, fmt.Sprintf("publisher.%s.%s", unsafeName.ReplaceAllString(identity, "_"), tr.SID))
	sent := tr.Sent()

	h := corpus.NewSyntheticHeader(tr.Kind, 0, "tests/e2e publisher")
	h.TrackID = tr.SID
	h.EpochUnixUs = tr.Start().UnixMicro()
	h.Note = "sent schedule; withheld packets omitted"

	trace := &corpus.Trace{Header: h}
	var dropped []uint16

	for _, s := range sent {
		if s.Dropped {
			dropped = append(dropped, s.Seq)
			continue
		}

		trace.Records = append(trace.Records, corpus.Record{
			T: corpus.RecordRTP, AtUs: s.At.Microseconds(), Seq: s.Seq, TS: s.TS, M: s.Marker,
			PT: tr.opts.Schedule.Records[0].PT, PLen: s.PLen,
		})
	}

	if err := trace.WriteFile(base + ".sent.ndjson.gz"); err != nil {
		t.Logf("publisher artifacts: %v", err)
	}

	summary := map[string]any{
		"trackId":  tr.SID,
		"kind":     tr.Kind,
		"name":     tr.opts.Name,
		"start":    tr.Start(),
		"packets":  len(sent),
		"withheld": dropped,
		"plisAtMs": durationsMs(tr.PLIs()),
		"nacks":    tr.NACKs(),
	}

	if data, err := json.MarshalIndent(summary, "", "  "); err == nil {
		_ = os.WriteFile(base+".summary.json", data, 0o644)
	}
}

func durationsMs(ds []time.Duration) []float64 {
	out := make([]float64, len(ds))
	for i, d := range ds {
		out[i] = float64(d.Microseconds()) / 1000
	}

	return out
}

// scheduleFrames groups a schedule's rtp records into frames by timestamp.
func scheduleFrames(recs []corpus.Record, vp8 bool) []schedFrame {
	var frames []schedFrame

	for _, r := range recs {
		if r.T != corpus.RecordRTP || r.RTX || r.Dup {
			continue
		}

		if n := len(frames); n == 0 || frames[n-1].ts != r.TS {
			frames = append(frames, schedFrame{atUs: r.AtUs, ts: r.TS, key: vp8 && isVP8KeyFrameStart(r.Pay)})
		}

		f := &frames[len(frames)-1]
		f.recs = append(f.recs, r)
	}

	return frames
}

// loopSpan is one pass of the schedule plus one frame interval, in µs and
// RTP ticks, so the next pass continues at the schedule's frame rate.
func loopSpan(frames []schedFrame) (int64, uint32) {
	first, last := frames[0], frames[len(frames)-1]
	if len(frames) == 1 {
		return 20000, 960
	}

	n := int64(len(frames) - 1)
	us := last.atUs - first.atUs
	ts := last.ts - first.ts

	return us + us/n, ts + ts/uint32(n)
}

// packetOffsets gives each packet's offset from its frame's first: spread
// evenly over spread when it is set, else the schedule's own.
func packetOffsets(recs []corpus.Record, spread time.Duration) []time.Duration {
	out := make([]time.Duration, len(recs))

	for j, r := range recs {
		switch {
		case spread > 0 && len(recs) > 1:
			out[j] = spread * time.Duration(j) / time.Duration(len(recs)-1)
		default:
			out[j] = time.Duration(r.AtUs-recs[0].AtUs) * time.Microsecond
		}
	}

	return out
}

// templateKeyFrame is the schedule's first key frame, whose packets answer
// PLIs, or a generated one if the schedule has none.
func templateKeyFrame(t *testing.T, frames []schedFrame, width, height int) []corpus.Record {
	for _, f := range frames {
		if f.key {
			return f.recs
		}
	}

	g, err := corpus.GenerateVP8(corpus.VP8Options{FPS: 1, Frames: 1, Width: width, Height: height})
	require.NoError(t, err)

	return g.Records
}

// vp8DescriptorLen is the length of the RFC 7741 payload descriptor at p, or
// 0 if p is too short to hold it.
func vp8DescriptorLen(p []byte) int {
	if len(p) < 1 {
		return 0
	}

	n := 1
	if p[0]&0x80 == 0 {
		return n
	}

	if len(p) < 2 {
		return 0
	}

	x := p[1]
	n++

	if x&0x80 != 0 { // I: picture ID, 7 or 15 bits
		if len(p) <= n {
			return 0
		}

		if p[n]&0x80 != 0 {
			n += 2
		} else {
			n++
		}
	}

	if x&0x40 != 0 { // L: TL0PICIDX
		n++
	}

	if x&0x30 != 0 { // T or K: TID/Y/KEYIDX
		n++
	}

	if len(p) < n {
		return 0
	}

	return n
}

// isVP8KeyFrameStart reports whether p starts a VP8 key frame: S set,
// partition 0, and the frame tag's inverse key frame bit clear.
func isVP8KeyFrameStart(p []byte) bool {
	n := vp8DescriptorLen(p)

	return n > 0 && len(p) > n && p[0]&0x10 != 0 && p[0]&0x07 == 0 && p[n]&0x01 == 0
}

func vp8PictureID(p []byte) (uint16, bool) {
	if len(p) < 3 || p[0]&0x80 == 0 || p[1]&0x80 == 0 {
		return 0, false
	}

	if p[2]&0x80 != 0 {
		if len(p) < 4 {
			return 0, false
		}

		return uint16(p[2]&0x7F)<<8 | uint16(p[3]), true
	}

	return uint16(p[2] & 0x7F), true
}

func setVP8PictureID(p []byte, id uint16) {
	if len(p) < 3 || p[0]&0x80 == 0 || p[1]&0x80 == 0 {
		return
	}

	if p[2]&0x80 != 0 {
		if len(p) >= 4 {
			p[2] = 0x80 | byte(id>>8)&0x7F
			p[3] = byte(id)
		}

		return
	}

	p[2] = byte(id) & 0x7F
}

// frameTag precedes the frame index tagFrame writes, so TaggedFrame can
// find it.
var frameTag = []byte("BBBF")

// tagFrame writes frameTag and frame into payload, the first packet of a
// frame, past the VP8 descriptor and frame header or the Opus TOC byte.
func tagFrame(payload []byte, kind corpus.Kind, key bool, frame int) {
	at := 1
	if kind == corpus.KindVideo {
		at = vp8DescriptorLen(payload) + 3
		if key {
			at += 7
		}
	}

	if len(payload) >= at+len(frameTag)+4 {
		copy(payload[at:], frameTag)
		binary.BigEndian.PutUint32(payload[at+len(frameTag):], uint32(frame))
	}
}

// TaggedFrame returns the frame index tagFrame wrote into a recorded
// frame's data, false if it has none.
func TaggedFrame(data []byte) (int, bool) {
	i := bytes.Index(data[:min(len(data), 64)], frameTag)
	if i < 0 || len(data) < i+len(frameTag)+4 {
		return 0, false
	}

	return int(binary.BigEndian.Uint32(data[i+len(frameTag):])), true
}

func sleepUntil(ctx context.Context, at time.Time) bool {
	d := time.Until(at)
	if d <= 0 {
		return ctx.Err() == nil
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func withDefault(v, def int) int {
	if v == 0 {
		return def
	}

	return v
}
