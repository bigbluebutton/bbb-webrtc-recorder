package corpus

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()

	gz, err := gzip.NewReader(bytes.NewReader(b))
	require.NoError(t, err)

	plain, err := io.ReadAll(gz)
	require.NoError(t, err)

	return plain
}

func eventTrace(t *testing.T) *Trace {
	t.Helper()

	tr, err := GenerateVP8(VP8Options{FPS: 30, Frames: 90, KeyInterval: 30, SpreadUs: 3000})
	require.NoError(t, err)

	tr.Header.Provenance = ProvenanceCapture
	at := tr.Records[100].AtUs
	tr.Records = slices.Insert(tr.Records, 101, Record{T: RecordPLI, AtUs: at, Reason: "packet_drop"}, Record{T: RecordTimeout, AtUs: at})
	tr.Records = append(tr.Records, Record{T: RecordEOF, AtUs: tr.Records[len(tr.Records)-1].AtUs + 1})
	require.Greater(t, len(tr.Records), 2*flushEvery)

	return tr
}

func TestWriterMatchesTraceWrite(t *testing.T) {
	tr := eventTrace(t)

	var batch bytes.Buffer
	require.NoError(t, tr.Write(&batch))

	var stream bytes.Buffer
	w, err := NewWriter(&stream, tr.Header)
	require.NoError(t, err)

	for _, r := range tr.Records {
		require.NoError(t, w.WriteRecord(r))
	}

	require.NoError(t, w.Close())
	assert.Equal(t, int64(stream.Len()), w.Written())

	got, err := Read(bytes.NewReader(stream.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, tr, got)
	assert.Equal(t, gunzip(t, batch.Bytes()), gunzip(t, stream.Bytes()), "the same NDJSON, only flushed differently")
}

func TestWriterConcurrent(t *testing.T) {
	const writers, each = 8, 200

	var buf bytes.Buffer
	w, err := NewWriter(&buf, NewSyntheticHeader(KindAudio, 1, "test"))
	require.NoError(t, err)

	var wg sync.WaitGroup
	for g := range writers {
		wg.Go(func() {
			for i := range each {
				r := Record{T: RecordRTP, Seq: uint16(g*each + i), PT: 111, PLen: 1}
				if i%50 == 0 {
					r = Record{T: RecordPLI, Reason: "direct_request"}
				}

				assert.NoError(t, w.WriteRecord(r))
				_ = w.Written()
			}
		})
	}
	wg.Wait()

	require.NoError(t, w.WriteRecord(Record{T: RecordEOF}))
	require.NoError(t, w.Close())

	got, err := Read(&buf)
	require.NoError(t, err)
	require.Len(t, got.Records, writers*each+1)

	seen := map[uint16]bool{}
	plis := 0
	for _, r := range got.Records {
		switch r.T {
		case RecordRTP:
			assert.False(t, seen[r.Seq], "seq %d written once", r.Seq)
			seen[r.Seq] = true
		case RecordPLI:
			plis++
		}
	}

	assert.Len(t, seen, writers*(each-each/50))
	assert.Equal(t, writers*each/50, plis)
}

func TestWriterWrittenMonotone(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, NewSyntheticHeader(KindVideo, 1, "test"))
	require.NoError(t, err)

	rng := rand.New(rand.NewPCG(1, 2))
	pay := make([]byte, 1200)
	prev := w.Written()
	assert.Positive(t, prev, "the gzip header is out once the trace header is")

	for i := range 10 * flushEvery {
		for j := range pay {
			pay[j] = byte(rng.Uint32())
		}

		require.NoError(t, w.WriteRecord(Record{T: RecordRTP, AtUs: int64(i), Seq: uint16(i), PT: 96, PLen: len(pay), Pay: pay}))

		n := w.Written()
		assert.GreaterOrEqual(t, n, prev, "record %d", i)
		assert.Equal(t, int64(buf.Len()), n, "every byte handed down is counted")

		if (i+1)%flushEvery == 0 {
			assert.Greater(t, n, int64((i+1)*len(pay)*9/10), "a flush leaves no incompressible payload behind")
		}

		prev = n
	}

	require.NoError(t, w.Close())
	assert.Greater(t, w.Written(), prev)
	assert.Equal(t, int64(buf.Len()), w.Written())
}

type failWriter struct{ after int }

func (f *failWriter) Write(p []byte) (int, error) {
	if f.after <= 0 {
		return 0, errors.New("disk full")
	}

	f.after--

	return len(p), nil
}

func TestWriterRejects(t *testing.T) {
	var buf bytes.Buffer
	bad := NewSyntheticHeader(KindVideo, 1, "test")
	bad.Provenance = ""

	_, err := NewWriter(&buf, bad)
	require.ErrorContains(t, err, "provenance is required")
	assert.Zero(t, buf.Len(), "nothing is written for an invalid header")

	w, err := NewWriter(&buf, NewSyntheticHeader(KindVideo, 1, "test"))
	require.NoError(t, err)

	require.NoError(t, w.WriteRecord(Record{T: RecordRTP, AtUs: 10, PT: 96}))
	require.ErrorContains(t, w.WriteRecord(Record{T: RecordTimeout, AtUs: 9}), "record 1: atUs 9 before the previous 10")
	require.ErrorContains(t, w.WriteRecord(Record{T: RecordPLI, AtUs: 10}), "record 1: pli without a reason")
	require.NoError(t, w.WriteRecord(Record{T: RecordTimeout, AtUs: 10}), "a rejected record leaves the writer usable")
	require.NoError(t, w.WriteRecord(Record{T: RecordEOF, AtUs: 11, Reason: "truncated"}))
	require.ErrorIs(t, w.WriteRecord(Record{T: RecordTimeout, AtUs: 12}), errAfterEOF)

	require.NoError(t, w.Close())
	require.NoError(t, w.Close(), "Close is idempotent")
	require.ErrorIs(t, w.WriteRecord(Record{T: RecordTimeout, AtUs: 12}), ErrWriterClosed)

	got, err := Read(&buf)
	require.NoError(t, err)
	assert.Equal(t, []Record{
		{T: RecordRTP, AtUs: 10, PT: 96},
		{T: RecordTimeout, AtUs: 10},
		{T: RecordEOF, AtUs: 11, Reason: "truncated"},
	}, got.Records)
}

func TestWriterIOErrorSticks(t *testing.T) {
	w, err := NewWriter(&failWriter{after: 1}, NewSyntheticHeader(KindAudio, 1, "test"))
	require.NoError(t, err, "the gzip header fits the first write")

	var first error
	for i := 0; first == nil && i < 10*flushEvery; i++ {
		first = w.WriteRecord(Record{T: RecordRTP, AtUs: int64(i), PT: 111})
	}

	require.ErrorContains(t, first, "disk full")
	assert.Equal(t, first, w.WriteRecord(Record{T: RecordTimeout, AtUs: 1 << 40}))
	assert.Equal(t, first, w.Close())
	assert.NoError(t, w.Close())
}
