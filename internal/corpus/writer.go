package corpus

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// flushEvery bounds how many records Written can lag behind. A flush ends a
// deflate block, so flushing each record would cost compression.
const flushEvery = 64

var (
	ErrWriterClosed = errors.New("corpus: writer closed")
	errAfterEOF     = errors.New("corpus: record after eof")
)

// Writer streams a trace as gzipped NDJSON, one record at a time, and is safe
// for concurrent use. Each record is validated as Trace.Validate would; an
// invalid record is rejected without being written and the Writer stays
// usable, while a write error is returned by every later call.
type Writer struct {
	mu         sync.Mutex
	gz         *gzip.Writer
	enc        *json.Encoder
	written    atomic.Int64
	flushEvery int
	pending    int
	n          int
	prevAt     int64
	ended      bool
	closed     bool
	err        error
}

// NewWriter validates h and writes it as the first line of a trace on w.
// Close the Writer to end the gzip stream; it does not close w.
func NewWriter(w io.Writer, h Header) (*Writer, error) {
	return newWriter(w, h, flushEvery)
}

// newWriter flushes the gzip stream every flush records, or only on Close if
// flush is 0, so Trace.Write keeps its unflushed byte-for-byte encoding.
func newWriter(w io.Writer, h Header, flush int) (*Writer, error) {
	if err := h.validate(); err != nil {
		return nil, err
	}

	tw := &Writer{flushEvery: flush}
	tw.gz = gzip.NewWriter(countingWriter{w: w, n: &tw.written})
	tw.enc = json.NewEncoder(tw.gz)

	if err := tw.enc.Encode(h); err != nil {
		return nil, err
	}

	return tw, nil
}

// WriteRecord appends r with its AtUs as given, rejecting one before the
// previous record's: concurrent callers keep their own records in order.
func (w *Writer) WriteRecord(r Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.write(r)
}

func (w *Writer) write(r Record) error {
	switch {
	case w.closed:
		return ErrWriterClosed
	case w.err != nil:
		return w.err
	case w.ended:
		return errAfterEOF
	}

	if err := validateRecord(w.n, r, w.prevAt); err != nil {
		return err
	}

	if err := w.enc.Encode(&r); err != nil {
		w.err = fmt.Errorf("record %d: %w", w.n, err)
		return w.err
	}

	w.n++
	w.prevAt = r.AtUs
	w.ended = r.T == RecordEOF

	if w.pending++; w.flushEvery > 0 && w.pending >= w.flushEvery {
		w.pending = 0

		if err := w.gz.Flush(); err != nil {
			w.err = fmt.Errorf("flush: %w", err)
			return w.err
		}
	}

	return nil
}

// Written returns the compressed bytes handed to the underlying writer so
// far, gzip header included. It never decreases, lags the records written
// by fewer than flushEvery records, and after Close is the length of the stream.
func (w *Writer) Written() int64 {
	return w.written.Load()
}

// Close ends the gzip stream; an eof record is the caller's to write. Later
// writes fail with ErrWriterClosed and later Close calls return nil.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}

	w.closed = true

	if w.err != nil {
		return w.err
	}

	return w.gz.Close()
}

type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))

	return n, err
}
