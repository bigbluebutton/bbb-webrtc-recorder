package recorder

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/pion/rtp"
)

const (
	h264NALSlice  = 1
	h264NALIDR    = 5
	h264NALAUD    = 9
	h264NALFiller = 12
	h264STAPA     = 24
	h264FUA       = 28

	// LiveKit sends 2x2 blank key frames when a track closes or changes
	// codec. Writing one would set the track to that size.
	h264MinKeyFrameSize = 16
	// VP8 key frames have the same cap. A larger SPS would set an absurd
	// track size.
	h264MaxKeyFrameSize = 8192
)

var (
	errH264Forbidden = errors.New("forbidden_zero_bit set")
	// errH264FUOpen is a cut. After a sequence gap, h264AccessUnits skips
	// the rest of the unit, so a NAL unit inside an open FU-A means no
	// packet was lost. When LiveKit closes a downtrack inside an FU-A, its
	// first blank frame takes the cut frame's timestamp, so both are one
	// access unit.
	errH264FUOpen = fmt.Errorf("NAL unit inside an open FU-A: %w", errCutFrame)
)

// h264AccessUnit is one H.264 access unit: the NAL units of one RTP timestamp.
type h264AccessUnit struct {
	// lost marks a dropped unit (see h264AccessUnits). It holds nothing else.
	lost      bool
	timestamp uint32
	// data holds every kept NAL unit with a 4-byte big-endian length prefix
	// (AVCC), the form Matroska blocks take with avcC lengthSizeMinusOne 3.
	data []byte
	idr  bool
	// slice reports at least one slice (NAL type 1 or 5).
	slice bool
	// sps and pps are the parameter sets in data, header byte included and
	// no length prefix. They share data's memory.
	sps, pps          [][]byte
	firstSeq, lastSeq uint16
}

// h264AccessUnits splits packets, the packets of one jitter-buffer sample in
// sequence order, at RTP timestamp changes and depacketizes each part. A
// malformed unit, a cut unit or a sequence gap leaves a lost unit in its
// place, and the units after it are still returned. The error describes the
// first loss that is not a cut, or else the first cut.
//
// The jitter buffer starts a sample at any NAL unit start. At a simulcast
// layer switch, LiveKit cuts the old layer's frame, and the new layer's key
// frame follows in the same sample. pion's H264Packet is not used: it keeps
// FU-A state across calls and ignores the start bit.
func h264AccessUnits(packets []*rtp.Packet) ([]h264AccessUnit, error) {
	var (
		units    []h264AccessUnit
		b        h264UnitBuilder
		open     bool
		firstErr error
		// While skipping, the packets of skipTS belong to a lost unit.
		skipping bool
		skipTS   uint32
	)
	lose := func(err error) {
		// Malformed input wins over a cut, which is only logged at Debug.
		if firstErr == nil || errors.Is(firstErr, errCutFrame) && !errors.Is(err, errCutFrame) {
			firstErr = err
		}
		if n := len(units); n == 0 || !units[n-1].lost {
			units = append(units, h264AccessUnit{lost: true})
		}
		open = false
	}
	finish := func() {
		u, err := b.finish()
		open = false
		if err != nil {
			lose(err)
			return
		}
		units = append(units, u)
	}
	for i, p := range packets {
		if p == nil {
			lose(fmt.Errorf("H.264 packet %d is nil", i))
			return units, firstErr
		}
		// A padding packet holds a sequence number, so it counts here.
		if i > 0 && p.SequenceNumber != packets[i-1].SequenceNumber+1 {
			gap := fmt.Errorf("H.264 sequence gap: %d follows %d", p.SequenceNumber, packets[i-1].SequenceNumber)
			// A unit that got its marker, followed by another timestamp, is
			// whole: the lost packets came after it. Any other open unit may
			// be missing some.
			if open && packets[i-1].Marker && !b.fuOpen && p.Timestamp != b.unit.timestamp {
				finish()
			} else if open {
				skipping, skipTS = true, b.unit.timestamp
			}
			lose(gap)
		}
		if p.Padding {
			continue
		}
		if skipping && p.Timestamp == skipTS {
			continue
		}
		skipping = false
		if open && p.Timestamp != b.unit.timestamp {
			if err := b.cut(); err != nil {
				lose(err)
			} else {
				finish()
			}
		}
		if !open {
			b = h264UnitBuilder{unit: h264AccessUnit{timestamp: p.Timestamp, firstSeq: p.SequenceNumber}}
			open = true
		}
		b.unit.lastSeq, b.marker = p.SequenceNumber, p.Marker
		if err := b.push(p.Payload); err != nil {
			lose(fmt.Errorf("H.264 access unit at timestamp %d, sequence number %d: %w",
				p.Timestamp, p.SequenceNumber, err))
			skipping, skipTS = true, p.Timestamp
		}
	}
	// The jitter buffer ends a sample at a marker or at a packet with no
	// payload, which it leaves out. LiveKit sends such padding after a frame
	// it cuts at a pause. A frame over the MTU goes as FU-A, so that cut often
	// falls inside one. An FU-A still open at the marker is not a cut: the
	// sender ended the frame there, and finish reports it as malformed.
	if open && (!b.fuOpen || !b.marker) {
		if err := b.cut(); err != nil {
			lose(err)
		}
	}
	if open {
		finish()
	}
	return units, firstErr
}

// h264UnitBuilder depacketizes the payloads of one access unit.
type h264UnitBuilder struct {
	unit h264AccessUnit
	// spsAt and ppsAt are the offsets of the parameter sets' length prefixes in data.
	spsAt, ppsAt []int
	fuOpen       bool
	fuType       byte
	// fuStart is the offset of the open FU-A NAL unit's length prefix.
	fuStart int
	// marker is the marker bit of the last packet pushed.
	marker bool
}

// cut returns errCutFrame when the unit ends inside an FU-A, or before the
// marker of a unit that holds a slice. Parameter sets alone are no frame, so
// they need no marker.
func (b *h264UnitBuilder) cut() error {
	switch {
	case b.fuOpen:
		return fmt.Errorf("H.264 access unit at timestamp %d: FU-A not ended: %w", b.unit.timestamp, errCutFrame)
	case b.unit.slice && !b.marker:
		return fmt.Errorf("H.264 access unit at timestamp %d: no marker: %w", b.unit.timestamp, errCutFrame)
	}
	return nil
}

func (b *h264UnitBuilder) push(payload []byte) error {
	if len(payload) == 0 {
		return errors.New("empty payload")
	}
	if payload[0]&0x80 != 0 {
		return errH264Forbidden
	}
	switch t := payload[0] & h264NALTypeMask; {
	case t >= 1 && t <= 23:
		if b.fuOpen {
			return errH264FUOpen
		}
		return b.appendNAL(payload)
	case t == h264STAPA:
		if b.fuOpen {
			return errH264FUOpen
		}
		return b.pushSTAPA(payload[1:])
	case t == h264FUA:
		return b.pushFUA(payload)
	case t >= 25 && t <= 29:
		return fmt.Errorf("packetization type %d not supported", t)
	default:
		return fmt.Errorf("reserved NAL unit type %d", t)
	}
}

func (b *h264UnitBuilder) pushSTAPA(rest []byte) error {
	if len(rest) == 0 {
		return errors.New("STAP-A holds no NAL units")
	}
	for len(rest) > 0 {
		if len(rest) < 2 {
			return errors.New("STAP-A size field truncated")
		}
		n := int(binary.BigEndian.Uint16(rest))
		rest = rest[2:]
		if n == 0 {
			return errors.New("STAP-A holds a zero-length NAL unit")
		}
		if n > len(rest) {
			return fmt.Errorf("STAP-A NAL unit of %d bytes overruns the %d bytes left", n, len(rest))
		}
		if err := h264CheckInnerHeader(rest[0], "STAP-A"); err != nil {
			return err
		}
		if err := b.appendNAL(rest[:n]); err != nil {
			return err
		}
		rest = rest[n:]
	}
	return nil
}

// pushFUA adds one FU-A fragment (RFC 6184 5.8).
func (b *h264UnitBuilder) pushFUA(payload []byte) error {
	if len(payload) < 2 {
		return errors.New("FU-A without an FU header")
	}
	header := payload[1]
	start, end, t := header&0x80 != 0, header&0x40 != 0, header&h264NALTypeMask
	if err := h264CheckInnerHeader(t, "FU-A"); err != nil {
		return err
	}
	if len(payload) == 2 {
		return errors.New("FU-A fragment without data")
	}
	if start && end {
		return errors.New("FU-A with both start and end bits")
	}
	switch {
	case start && b.fuOpen:
		return errors.New("FU-A start while another FU-A is open")
	case start:
		b.fuOpen, b.fuType, b.fuStart = true, t, len(b.unit.data)
		// The NAL unit header is the FU indicator's F and NRI with the FU header's type.
		b.unit.data = append(b.unit.data, 0, 0, 0, 0, payload[0]&0xe0|t)
	case !b.fuOpen:
		return errors.New("FU-A continuation without a start")
	case t != b.fuType:
		return fmt.Errorf("FU-A type changes from %d to %d", b.fuType, t)
	}
	b.unit.data = append(b.unit.data, payload[2:]...)
	if end {
		b.fuOpen = false
		at := b.fuStart
		if b.fuType == h264NALAUD || b.fuType == h264NALFiller {
			b.unit.data = b.unit.data[:at]
			return nil
		}
		binary.BigEndian.PutUint32(b.unit.data[at:], uint32(len(b.unit.data)-at-4))
		b.note(b.fuType, at)
	}
	return nil
}

func h264CheckInnerHeader(header byte, in string) error {
	if header&0x80 != 0 {
		return errH264Forbidden
	}
	if t := header & h264NALTypeMask; t == 0 || t > 23 {
		return fmt.Errorf("NAL unit type %d inside %s", t, in)
	}
	return nil
}

// appendNAL adds a whole NAL unit to data. AUD and filler carry no picture
// data, and Matroska blocks already delimit access units.
func (b *h264UnitBuilder) appendNAL(nal []byte) error {
	t := nal[0] & h264NALTypeMask
	if t == h264NALAUD || t == h264NALFiller {
		return nil
	}
	if (t == h264NALSlice || t == h264NALIDR) && len(nal) < 2 {
		return errors.New("slice NAL unit without a slice header")
	}
	at := len(b.unit.data)
	b.unit.data = binary.BigEndian.AppendUint32(b.unit.data, uint32(len(nal)))
	b.unit.data = append(b.unit.data, nal...)
	b.note(t, at)
	return nil
}

// note records a kept NAL unit of type t whose length prefix is at offset at.
func (b *h264UnitBuilder) note(t byte, at int) {
	switch t {
	case h264NALSlice:
		b.unit.slice = true
	case h264NALIDR:
		b.unit.slice, b.unit.idr = true, true
	case h264NALSPS:
		b.spsAt = append(b.spsAt, at)
	case h264NALPPS:
		b.ppsAt = append(b.ppsAt, at)
	}
}

func (b *h264UnitBuilder) finish() (h264AccessUnit, error) {
	if b.fuOpen {
		return h264AccessUnit{}, fmt.Errorf("H.264 access unit at timestamp %d: FU-A not ended", b.unit.timestamp)
	}
	u := b.unit
	u.sps = h264NALsAt(u.data, b.spsAt)
	u.pps = h264NALsAt(u.data, b.ppsAt)
	return u, nil
}

// h264NALsAt returns the NAL units whose length prefixes are at offsets in
// data. Each has its capacity cut to its length, so an append copies it.
func h264NALsAt(data []byte, offsets []int) [][]byte {
	var nals [][]byte
	for _, at := range offsets {
		end := at + 4 + int(binary.BigEndian.Uint32(data[at:]))
		nals = append(nals, data[at+4:end:end])
	}
	return nals
}

// h264KeyFrame returns an error unless u can start decoding: it holds an IDR
// slice, an SPS and a PPS. Every SPS must decode to a size from 16x16 to
// 8192x8192. It returns the size of the SPS the IDR slice uses, else the
// first SPS's. It also returns the avcC CodecPrivate built from u's own
// parameter sets.
func h264KeyFrame(u h264AccessUnit) (width, height int, codecPrivate []byte, err error) {
	if !u.idr || len(u.sps) == 0 || len(u.pps) == 0 {
		return 0, 0, nil, errUnusableKeyFrame
	}
	active, ok := h264ActiveSPSID(u)
	for i, nal := range u.sps {
		sps, err := parseH264SPS(nal)
		switch {
		case err != nil:
			return 0, 0, nil, fmt.Errorf("%w: %w", errUnreadableKeyFrame, err)
		case max(sps.width, sps.height) > h264MaxKeyFrameSize:
			return 0, 0, nil, fmt.Errorf("%w: %dx%d", errOversizedKeyFrame, sps.width, sps.height)
		case min(sps.width, sps.height) < h264MinKeyFrameSize:
			return 0, 0, nil, fmt.Errorf("%w: %dx%d", errUnusableKeyFrame, sps.width, sps.height)
		}
		if i == 0 || ok && sps.id == active {
			width, height = sps.width, sps.height
		}
	}
	codecPrivate, err = h264CodecPrivate(u.sps, u.pps)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("%w: %w", errAvcCKeyFrame, err)
	}
	return width, height, codecPrivate, nil
}

// h264ActiveSPSID returns the seq_parameter_set_id that u's first IDR slice
// uses: the slice names a PPS, and the PPS names an SPS. A later parameter
// set replaces an earlier one with the same id. ok is false when the
// slice's PPS is not in u, or an id cannot be read.
func h264ActiveSPSID(u h264AccessUnit) (id uint32, ok bool) {
	for data := u.data; len(data) > 0; {
		end := 4 + int(binary.BigEndian.Uint32(data))
		nal := data[4:end]
		data = data[end:]
		if nal[0]&h264NALTypeMask != h264NALIDR {
			continue
		}
		// first_mb_in_slice, slice_type, pic_parameter_set_id
		r := &h264BitReader{b: h264RBSP(nal[1:])}
		r.ue()
		r.ue()
		ppsID := r.ue()
		if r.err != nil {
			return 0, false
		}
		for _, pps := range u.pps {
			pr := &h264BitReader{b: h264RBSP(pps[1:])}
			if pid, sid := pr.ue(), pr.ue(); pr.err == nil && pid == ppsID {
				id, ok = sid, true
			}
		}
		return id, ok
	}
	return 0, false
}

// isH264KeyFrameStart reports whether p starts a key frame the way LiveKit's
// SFU detects one. Such a packet carries an SPS, alone, inside a STAP-A, or
// as an FU-A start. LiveKit starts forwarding and switches layers at such a
// packet.
func isH264KeyFrameStart(p *rtp.Packet) bool {
	if p == nil || p.Padding || len(p.Payload) == 0 {
		return false
	}
	payload := p.Payload
	switch payload[0] & h264NALTypeMask {
	case h264NALSPS:
		return true
	case h264STAPA:
		for rest := payload[1:]; len(rest) >= 2; {
			n := int(binary.BigEndian.Uint16(rest))
			rest = rest[2:]
			if n == 0 || n > len(rest) {
				return false
			}
			if rest[0]&h264NALTypeMask == h264NALSPS {
				return true
			}
			rest = rest[n:]
		}
	case h264FUA:
		return len(payload) >= 2 && payload[1]&0x80 != 0 && payload[1]&h264NALTypeMask == h264NALSPS
	}
	return false
}

// h264Frames is CodecH264's frames: a frame for each access unit that holds
// a slice, and a lost one for each lost unit. Parameter sets alone, which
// LiveKit forwards at a simulcast start, are no frame. An IDR unit that
// cannot start decoding is lost too.
func h264Frames(packets []*rtp.Packet) ([]videoFrame, error) {
	units, err := h264AccessUnits(packets)

	frames := make([]videoFrame, 0, len(units))
	for _, u := range units {
		switch {
		case u.lost:
			frames = append(frames, videoFrame{lost: true})
		case !u.slice:
		case u.idr:
			width, height, codecPrivate, keyErr := h264KeyFrame(u)
			if keyErr != nil {
				frames = append(frames, videoFrame{lost: true})
				// After a layer switch, the cut frame must not hide the
				// new layer's refused key frame.
				if err == nil || routineFrameError(err) && !routineFrameError(keyErr) {
					err = fmt.Errorf("IDR at timestamp %d: %w", u.timestamp, keyErr)
				}

				continue
			}

			frames = append(frames, videoFrame{
				data: u.data, timestamp: u.timestamp,
				keyFrame: true, width: width, height: height, codecPrivate: codecPrivate,
			})
		default:
			frames = append(frames, videoFrame{data: u.data, timestamp: u.timestamp})
		}
	}

	return frames, err
}
