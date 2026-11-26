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
	errH264FUOpen    = errors.New("NAL unit inside an open FU-A")
)

// h264AccessUnit is one H.264 access unit: the NAL units of one RTP timestamp.
type h264AccessUnit struct {
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
// sequence order, at RTP timestamp changes and depacketizes each part. It
// returns the well-formed units before the first malformed one, and an error
// describing that one; units after it are not returned.
//
// The jitter buffer starts a sample at any NAL unit start, so after a loss a
// sample can hold a partial access unit. This catches the structurally broken
// ones. pion's H264Packet is not used: it keeps FU-A state across calls and
// ignores the start bit.
func h264AccessUnits(packets []*rtp.Packet) ([]h264AccessUnit, error) {
	var (
		units []h264AccessUnit
		b     h264UnitBuilder
		open  bool
	)
	for i, p := range packets {
		if p == nil {
			return units, fmt.Errorf("H.264 packet %d is nil", i)
		}
		// A padding packet holds a sequence number, so it counts here. A gap
		// may hide the end of the open unit, so that unit fails.
		if i > 0 && p.SequenceNumber != packets[i-1].SequenceNumber+1 {
			return units, fmt.Errorf("H.264 sequence gap: %d follows %d",
				p.SequenceNumber, packets[i-1].SequenceNumber)
		}
		if p.Padding {
			continue
		}
		if open && p.Timestamp != b.unit.timestamp {
			u, err := b.finish()
			if err != nil {
				return units, err
			}
			units = append(units, u)
			open = false
		}
		if !open {
			b = h264UnitBuilder{unit: h264AccessUnit{timestamp: p.Timestamp, firstSeq: p.SequenceNumber}}
			open = true
		}
		b.unit.lastSeq = p.SequenceNumber
		if err := b.push(p.Payload); err != nil {
			return units, fmt.Errorf("H.264 access unit at timestamp %d, sequence number %d: %w",
				p.Timestamp, p.SequenceNumber, err)
		}
	}
	if open {
		u, err := b.finish()
		if err != nil {
			return units, err
		}
		units = append(units, u)
	}
	return units, nil
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
		b.appendNAL(payload)
		return nil
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
		b.appendNAL(rest[:n])
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
func (b *h264UnitBuilder) appendNAL(nal []byte) {
	t := nal[0] & h264NALTypeMask
	if t == h264NALAUD || t == h264NALFiller {
		return
	}
	at := len(b.unit.data)
	b.unit.data = binary.BigEndian.AppendUint32(b.unit.data, uint32(len(nal)))
	b.unit.data = append(b.unit.data, nal...)
	b.note(t, at)
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

// h264KeyFrame reports whether u can start decoding: it holds an IDR slice,
// an SPS and a PPS. Every SPS must decode to a size from 16x16 to
// 8192x8192. It returns the first SPS's cropped size and the avcC
// CodecPrivate built from u's own parameter sets.
func h264KeyFrame(u h264AccessUnit) (width, height int, codecPrivate []byte, ok bool) {
	if !u.idr || len(u.sps) == 0 || len(u.pps) == 0 {
		return 0, 0, nil, false
	}
	for i, nal := range u.sps {
		sps, err := parseH264SPS(nal)
		if err != nil || min(sps.width, sps.height) < h264MinKeyFrameSize ||
			max(sps.width, sps.height) > h264MaxKeyFrameSize {
			return 0, 0, nil, false
		}
		if i == 0 {
			width, height = sps.width, sps.height
		}
	}
	codecPrivate, err := h264CodecPrivate(u.sps, u.pps)
	if err != nil {
		return 0, 0, nil, false
	}
	return width, height, codecPrivate, true
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
