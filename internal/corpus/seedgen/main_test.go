package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const corpusDir = "../../webrtc/livekit/testdata/corpus"

// The traces are compared decoded, since gzip output may change between Go
// releases. Captured traces have no generator and are skipped.
func TestCorpusMatchesGenerator(t *testing.T) {
	dir := t.TempDir()
	writeSeeds(dir, io.Discard)

	generated, err := filepath.Glob(filepath.Join(dir, "*.ndjson.gz"))
	require.NoError(t, err)
	require.NotEmpty(t, generated)

	for _, path := range generated {
		name := filepath.Base(path)
		want, err := corpus.ReadFile(filepath.Join(corpusDir, name))
		if !assert.NoError(t, err, "%s: generated but not in the corpus", name) {
			continue
		}
		got, err := corpus.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, want.Header, got.Header, "%s: header", name)
		if i := firstDiff(want.Records, got.Records); i >= 0 {
			t.Errorf("%s: records differ from index %d of %d committed, %d generated; regenerate with go run ./internal/corpus/seedgen",
				name, i, len(want.Records), len(got.Records))
		}
	}

	committed, err := filepath.Glob(filepath.Join(corpusDir, "*.ndjson.gz"))
	require.NoError(t, err)
	for _, path := range committed {
		name := filepath.Base(path)
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			continue
		}
		tr, err := corpus.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, corpus.ProvenanceCapture, tr.Header.Provenance,
			"%s: not generated, so it must be a capture", name)
	}
}

// firstDiff returns the index of the first record that differs, or -1.
func firstDiff(a, b []corpus.Record) int {
	for i := range min(len(a), len(b)) {
		if !reflect.DeepEqual(a[i], b[i]) {
			return i
		}
	}
	if len(a) != len(b) {
		return min(len(a), len(b))
	}

	return -1
}
