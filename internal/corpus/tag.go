package corpus

import (
	"bytes"
	"encoding/binary"
)

// frameTag precedes the value TagFrame writes, so TaggedFrame can find it.
var frameTag = []byte("BBBF")

// VP8DescriptorLen is the length of the RFC 7741 payload descriptor at p, or
// 0 if p is too short to hold it.
func VP8DescriptorLen(p []byte) int {
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

// VP8FrameStart returns the length of p's payload descriptor if p starts a
// VP8 frame: S set, partition 0, and a byte past the descriptor. Otherwise
// it returns zero. key is the inverse key frame bit, clear.
func VP8FrameStart(p []byte) (n int, key bool) {
	if len(p) == 0 || p[0]&0x10 == 0 || p[0]&0x07 != 0 {
		return 0, false
	}

	n = VP8DescriptorLen(p)
	if n == 0 || len(p) <= n {
		return 0, false
	}

	return n, p[n]&0x01 == 0
}

// VP8PictureID returns the picture ID of the descriptor at p, and its
// length in bits: 7 or 15, or 0 if p has none.
func VP8PictureID(p []byte) (id uint16, bits int) {
	if len(p) < 3 || p[0]&0x80 == 0 || p[1]&0x80 == 0 {
		return 0, 0
	}

	if p[2]&0x80 != 0 {
		if len(p) < 4 {
			return 0, 0
		}

		return uint16(p[2]&0x7F)<<8 | uint16(p[3]), 15
	}

	return uint16(p[2] & 0x7F), 7
}

// SetVP8PictureID writes id into the picture ID of the descriptor at p, in
// the ID's length there. A payload with no picture ID is left as it is.
func SetVP8PictureID(p []byte, id uint16) {
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

// TagFrame writes frameTag and v into payload, the first packet of a frame,
// past the VP8 descriptor and frame header or the Opus TOC byte. The
// recorder does not change those bytes, so the tag reaches the WebM block
// as it is. It reports whether it wrote the tag: a payload too short for
// it is left as it is.
func TagFrame(payload []byte, kind Kind, key bool, v uint32) bool {
	at := 1
	if kind == KindVideo {
		at = VP8DescriptorLen(payload) + 3
		if key {
			at += 7
		}
	}

	if len(payload) < at+len(frameTag)+4 {
		return false
	}

	copy(payload[at:], frameTag)
	binary.BigEndian.PutUint32(payload[at+len(frameTag):], v)

	return true
}

// TaggedFrame returns the value TagFrame wrote into a recorded frame's
// data, false if it has none.
func TaggedFrame(data []byte) (uint32, bool) {
	i := bytes.Index(data[:min(len(data), 64)], frameTag)
	if i < 0 || len(data) < i+len(frameTag)+4 {
		return 0, false
	}

	return binary.BigEndian.Uint32(data[i+len(frameTag):]), true
}
