package corpus

import (
	"bytes"
	"compress/gzip"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteReadRoundTrip(t *testing.T) {
	seed := int64(42)
	tr := validTrace()
	tr.Header.TrackID = "TR_video"
	tr.Header.Seed = &seed
	tr.Header.Note = "round trip"
	tr.Records[1].RTX = true

	var buf bytes.Buffer
	require.NoError(t, tr.Write(&buf))
	assert.Equal(t, []byte{0x1f, 0x8b}, buf.Bytes()[:2], "Write gzips")

	got, err := Read(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, tr, got)

	var again bytes.Buffer
	require.NoError(t, got.Write(&again))
	assert.Equal(t, buf.Bytes(), again.Bytes(), "the encoding is byte-stable")
}

func TestWriteReadFile(t *testing.T) {
	tr, err := GenerateVP8(VP8Options{FPS: 30, Frames: 20, KeyInterval: 10})
	require.NoError(t, err)

	path := filepath.Join(t.TempDir(), "cam.ndjson.gz")
	require.NoError(t, tr.WriteFile(path))

	got, err := ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, tr, got)
}

func TestReadPlainAndGzip(t *testing.T) {
	const plain = `{"v":1,"kind":"audio","codec":"opus","clockRate":48000,"ssrc":9,"provenance":"pcap-extract"}
{"t":"rtp","atUs":0,"seq":65535,"ts":0,"m":true,"pt":111,"plen":3,"pay":"/A=="}
{"t":"rtp","atUs":20000,"seq":0,"ts":960,"m":false,"pt":111,"plen":3}
{"t":"eof","atUs":30000,"seq":0,"ts":0,"m":false,"pt":0,"plen":0}
`
	want := &Trace{
		Header: Header{V: 1, Kind: KindAudio, Codec: CodecOpus, ClockRate: 48000, SSRC: 9, Provenance: ProvenancePcapExtract},
		Records: []Record{
			{T: RecordRTP, AtUs: 0, Seq: 65535, M: true, PT: 111, PLen: 3, Pay: []byte{0xFC}},
			{T: RecordRTP, AtUs: 20000, Seq: 0, TS: 960, PT: 111, PLen: 3},
			{T: RecordEOF, AtUs: 30000},
		},
	}

	got, err := Read(strings.NewReader(plain))
	require.NoError(t, err)
	assert.Equal(t, want, got)

	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, err = w.Write([]byte(plain))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	got, err = Read(&gz)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestReadErrors(t *testing.T) {
	const header = `{"v":1,"kind":"video","codec":"vp8","clockRate":90000,"ssrc":1,"provenance":"synthetic"}` + "\n"

	cases := []struct {
		name, in, want string
	}{
		{"empty", "", "missing header"},
		{"one byte", "{", "line 1"},
		{"no provenance", `{"v":1,"kind":"video","codec":"vp8","clockRate":90000,"ssrc":1}`, "provenance is required"},
		{"unknown field", header + `{"t":"rtp","readUs":5,"seq":1,"ts":0,"m":false,"pt":96,"plen":1}`, `line 2: json: unknown field "readUs"`},
		{"trailing data", header + `{"t":"eof","atUs":0} {}`, "line 2: trailing data"},
		{"blank line", header + "\n" + `{"t":"eof","atUs":0}`, "line 2"},
		{"bad base64", header + `{"t":"rtp","atUs":0,"pay":"!!","plen":4}`, "line 2"},
		{"invalid records", header + `{"t":"rtp","atUs":5,"plen":1}` + "\n" + `{"t":"rtp","atUs":4,"plen":1}`, "record 1: atUs 4"},
		{"bad gzip", "\x1f\x8b\x00", "gzip"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Read(strings.NewReader(c.in))
			assert.ErrorContains(t, err, c.want)
		})
	}
}

func TestWriteRejectsInvalid(t *testing.T) {
	tr := validTrace()
	tr.Header.Provenance = ""

	var buf bytes.Buffer
	assert.ErrorContains(t, tr.Write(&buf), "provenance is required")
	assert.Zero(t, buf.Len(), "nothing is written for an invalid trace")

	_, err := ReadFile(filepath.Join(t.TempDir(), "missing.ndjson.gz"))
	assert.Error(t, err)
}
