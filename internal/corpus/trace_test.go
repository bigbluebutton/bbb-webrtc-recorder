package corpus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validTrace() *Trace {
	return &Trace{
		Header: NewSyntheticHeader(KindVideo, 1234, "test"),
		Records: []Record{
			{T: RecordRTP, AtUs: 0, Seq: 1, TS: 10, PT: 96, PLen: 5, Pay: []byte{0x90}},
			{T: RecordRTP, AtUs: 0, Seq: 2, TS: 10, M: true, PT: 96, PLen: 5},
			{T: RecordEOF, AtUs: 100},
		},
	}
}

func TestValidate(t *testing.T) {
	require.NoError(t, validTrace().Validate())

	audio := &Trace{Header: NewSyntheticHeader(KindAudio, 1, "")}
	assert.NoError(t, audio.Validate(), "an empty audio trace is valid")

	cases := []struct {
		name   string
		mutate func(*Trace)
		want   string
	}{
		{"version", func(t *Trace) { t.Header.V = 2 }, "unsupported version 2"},
		{"kind/codec pair", func(t *Trace) { t.Header.Codec = CodecOpus }, "unsupported kind/codec"},
		{"unknown codec", func(t *Trace) { t.Header.Codec = "vp9" }, "unsupported kind/codec"},
		{"clock rate", func(t *Trace) { t.Header.ClockRate = 48000 }, "vp8 clock rate 48000"},
		{"missing provenance", func(t *Trace) { t.Header.Provenance = "" }, "provenance is required"},
		{"unknown provenance", func(t *Trace) { t.Header.Provenance = "field" }, "unknown provenance"},
		{"negative atUs", func(t *Trace) { t.Records[1].AtUs = -1 }, "negative atUs"},
		{"atUs not monotone", func(t *Trace) { t.Records[0].AtUs = 5 }, "record 1: atUs 0 before the previous 5"},
		{"plen short", func(t *Trace) { t.Records[0].PLen = 0 }, "plen 0 shorter than pay"},
		{"eof not last", func(t *Trace) { t.Records[1].T = RecordEOF }, "record 1: eof before the last"},
		{"unknown type", func(t *Trace) { t.Records[0].T = "pli" }, "unknown type"},
		{"payload type", func(t *Trace) { t.Records[0].PT = 200 }, "payload type 200"},
		{"rtx and dup", func(t *Trace) { t.Records[0].RTX, t.Records[0].Dup = true, true }, "both rtx and dup"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := validTrace()
			c.mutate(tr)
			assert.ErrorContains(t, tr.Validate(), c.want)
		})
	}
}

func TestRecordPacket(t *testing.T) {
	r := Record{T: RecordRTP, Seq: 65535, TS: 4294967295, M: true, PT: 111, PLen: 6, Pay: []byte{0xFC, 0x01}}
	p := r.Packet()

	require.NotNil(t, p)
	assert.Equal(t, uint8(2), p.Version)
	assert.Equal(t, uint16(65535), p.SequenceNumber)
	assert.Equal(t, uint32(4294967295), p.Timestamp)
	assert.True(t, p.Marker)
	assert.Equal(t, uint8(111), p.PayloadType)
	assert.Equal(t, []byte{0xFC, 0x01, 0, 0, 0, 0}, p.Payload)

	p.Payload[0] = 0
	assert.Equal(t, byte(0xFC), r.Pay[0], "the packet must not alias the record's payload")

	assert.Nil(t, Record{T: RecordEOF, AtUs: 5}.Packet())
}
