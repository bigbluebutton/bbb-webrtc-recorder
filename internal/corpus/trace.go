// Package corpus reads, writes, generates and mutates RTP read traces
// (format v1: gzipped NDJSON, a header line followed by one record per line).
package corpus

import (
	"errors"
	"fmt"

	"github.com/pion/rtp"
)

// Version is the trace format version this package reads and writes.
const Version = 1

// Kind is the media kind of a traced track.
type Kind string

// Codec is the RTP payload codec of a traced track.
type Codec string

// Provenance says where a trace's packets came from.
type Provenance string

// RecordType tells an RTP record from an end-of-stream record.
type RecordType string

const (
	KindVideo Kind = "video"
	KindAudio Kind = "audio"

	CodecVP8  Codec = "vp8"
	CodecOpus Codec = "opus"

	ProvenanceSynthetic   Provenance = "synthetic"
	ProvenancePcapExtract Provenance = "pcap-extract"
	ProvenanceFakeDevice  Provenance = "capture:fake-device"

	RecordRTP RecordType = "rtp"
	RecordEOF RecordType = "eof"

	vp8ClockRate  = 90000
	opusClockRate = 48000
)

// Header is the first line of a trace and describes the traced track.
type Header struct {
	V          int        `json:"v"`
	Kind       Kind       `json:"kind"`
	Codec      Codec      `json:"codec"`
	ClockRate  uint32     `json:"clockRate"`
	SSRC       uint32     `json:"ssrc"`
	TrackID    string     `json:"trackId,omitempty"`
	Provenance Provenance `json:"provenance"`
	Generator  string     `json:"generator,omitempty"`
	Seed       *int64     `json:"seed,omitempty"`
	Note       string     `json:"note,omitempty"`
}

// Record is one packet read from the track, or the end of the stream.
type Record struct {
	T RecordType `json:"t"`
	// AtUs is the read-time offset from the start of the trace, not socket arrival.
	AtUs int64  `json:"atUs"`
	Seq  uint16 `json:"seq"`
	TS   uint32 `json:"ts"`
	M    bool   `json:"m"`
	PT   uint8  `json:"pt"`
	RTX  bool   `json:"rtx,omitempty"`
	Dup  bool   `json:"dup,omitempty"`
	PLen int    `json:"plen"`
	// Pay is a payload prefix (base64 in JSON); replay zero-pads it to PLen.
	Pay []byte `json:"pay,omitempty"`
}

// Trace is a header plus its records in read order.
type Trace struct {
	Header  Header
	Records []Record
}

// NewSyntheticHeader returns a v1 header for a synthetic VP8 video or Opus audio track.
func NewSyntheticHeader(kind Kind, ssrc uint32, generator string) Header {
	h := Header{V: Version, Kind: kind, SSRC: ssrc, Provenance: ProvenanceSynthetic, Generator: generator}

	switch kind {
	case KindVideo:
		h.Codec, h.ClockRate = CodecVP8, vp8ClockRate
	case KindAudio:
		h.Codec, h.ClockRate = CodecOpus, opusClockRate
	}

	return h
}

// Packet returns the record as an RTP packet with its payload zero-padded to
// PLen, or nil for a non-RTP record; SSRC is left zero, the header carries it.
func (r Record) Packet() *rtp.Packet {
	if r.T != RecordRTP {
		return nil
	}

	payload := make([]byte, max(r.PLen, len(r.Pay)))
	copy(payload, r.Pay)

	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         r.M,
			PayloadType:    r.PT,
			SequenceNumber: r.Seq,
			Timestamp:      r.TS,
		},
		Payload: payload,
	}
}

// Validate checks the header and the record invariants replay relies on.
func (t *Trace) Validate() error {
	if err := t.Header.validate(); err != nil {
		return err
	}

	return validateRecords(t.Records)
}

func (h Header) validate() error {
	if h.V != Version {
		return fmt.Errorf("header: unsupported version %d", h.V)
	}

	switch {
	case h.Kind == KindVideo && h.Codec == CodecVP8:
		if h.ClockRate != vp8ClockRate {
			return fmt.Errorf("header: vp8 clock rate %d, want %d", h.ClockRate, vp8ClockRate)
		}
	case h.Kind == KindAudio && h.Codec == CodecOpus:
		if h.ClockRate != opusClockRate {
			return fmt.Errorf("header: opus clock rate %d, want %d", h.ClockRate, opusClockRate)
		}
	default:
		return fmt.Errorf("header: unsupported kind/codec %q/%q", h.Kind, h.Codec)
	}

	switch h.Provenance {
	case ProvenanceSynthetic, ProvenancePcapExtract, ProvenanceFakeDevice:
	case "":
		return errors.New("header: provenance is required")
	default:
		return fmt.Errorf("header: unknown provenance %q", h.Provenance)
	}

	return nil
}

func validateRecords(recs []Record) error {
	var prev int64

	for i, r := range recs {
		switch r.T {
		case RecordRTP:
		case RecordEOF:
			if i != len(recs)-1 {
				return fmt.Errorf("record %d: eof before the last record", i)
			}
		default:
			return fmt.Errorf("record %d: unknown type %q", i, r.T)
		}

		if r.AtUs < 0 {
			return fmt.Errorf("record %d: negative atUs %d", i, r.AtUs)
		}

		if r.AtUs < prev {
			return fmt.Errorf("record %d: atUs %d before the previous %d", i, r.AtUs, prev)
		}

		prev = r.AtUs

		if r.PT > 127 {
			return fmt.Errorf("record %d: payload type %d out of range", i, r.PT)
		}

		if r.PLen < len(r.Pay) {
			return fmt.Errorf("record %d: plen %d shorter than pay (%d bytes)", i, r.PLen, len(r.Pay))
		}

		if r.RTX && r.Dup {
			return fmt.Errorf("record %d: both rtx and dup", i)
		}
	}

	return nil
}
