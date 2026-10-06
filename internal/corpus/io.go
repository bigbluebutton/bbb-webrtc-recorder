package corpus

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// A full-payload record carries at most a ~1500-byte payload, about 2 KB in base64.
const maxLineBytes = 1 << 20

// Read parses and validates a trace, gzipped or plain NDJSON.
func Read(r io.Reader) (*Trace, error) {
	br := bufio.NewReader(r)
	src := io.Reader(br)

	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer gz.Close() //nolint:errcheck // read-only; a corrupt stream already fails the scan

		src = gz
	}

	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)

	t := &Trace{}
	line := 0

	for sc.Scan() {
		line++

		var err error
		if line == 1 {
			err = decodeStrict(sc.Bytes(), &t.Header)
		} else {
			var rec Record
			if err = decodeStrict(sc.Bytes(), &rec); err == nil {
				t.Records = append(t.Records, rec)
			}
		}

		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
	}

	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("line %d: %w", line+1, err)
	}

	if line == 0 {
		return nil, errors.New("empty trace: missing header")
	}

	if err := t.Validate(); err != nil {
		return nil, err
	}

	return t, nil
}

// ReadFile reads and validates the trace at path.
func ReadFile(path string) (*Trace, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only

	t, err := Read(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return t, nil
}

// Write validates the trace and writes it as gzipped NDJSON.
func (t *Trace) Write(w io.Writer) error {
	if err := t.Validate(); err != nil {
		return err
	}

	tw, err := newWriter(w, t.Header, 0)
	if err != nil {
		return err
	}

	for _, r := range t.Records {
		if err := tw.WriteRecord(r); err != nil {
			return err
		}
	}

	return tw.Close()
}

// WriteFile validates the trace and writes it to path as gzipped NDJSON.
func (t *Trace) WriteFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}

	bw := bufio.NewWriter(f)
	if err := t.Write(bw); err != nil {
		_ = f.Close()
		return err
	}

	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return err
	}

	return f.Close()
}

// decodeStrict rejects unknown fields so a misspelt key (readUs for atUs)
// fails loudly instead of decoding to zero.
func decodeStrict(line []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		return err
	}

	if dec.More() {
		return errors.New("trailing data after the JSON object")
	}

	return nil
}
