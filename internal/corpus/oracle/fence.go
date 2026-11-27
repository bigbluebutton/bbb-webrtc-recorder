package oracle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"sync"
)

// Fence pins values a test measures, by name, in a JSON file. A change to
// what the recorder writes then fails the test that measured it, even when
// every other check still passes. Running with update pins the values the
// run measures instead.
type Fence struct {
	path   string
	update bool
	want   map[string]json.RawMessage

	mu  sync.Mutex
	got map[string]json.RawMessage
}

// NewFence reads the values pinned in path. A missing file pins nothing,
// so that a first run with update can write it.
func NewFence(path string, update bool) (*Fence, error) {
	f := &Fence{path: path, update: update, want: map[string]json.RawMessage{}, got: map[string]json.RawMessage{}}

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return f, nil
	case err != nil:
		return nil, err
	}

	if err := json.Unmarshal(data, &f.want); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	return f, nil
}

// Check records v under name. It returns a description of the difference
// from the pinned value, or "" when they match or the fence is updating.
func (f *Fence) Check(name string, v any) (string, error) {
	got, err := json.Marshal(v)
	if err != nil {
		return "", err
	}

	f.mu.Lock()
	f.got[name] = got
	f.mu.Unlock()

	if f.update {
		return "", nil
	}

	want, ok := f.want[name]
	if !ok {
		return fmt.Sprintf("%s: no value pinned in %s; run with -update to pin %s", name, f.path, got), nil
	}

	var a, b bytes.Buffer
	if err := json.Compact(&a, want); err != nil {
		return "", err
	}
	if err := json.Compact(&b, got); err != nil {
		return "", err
	}

	if a.String() != b.String() {
		return fmt.Sprintf("%s: pinned %s, measured %s", name, a.String(), b.String()), nil
	}

	return "", nil
}

// Close returns the pinned names that no Check measured. When updating, it
// writes the measured values over the pins it read, so a -run filter keeps
// the others.
func (f *Fence) Close() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var unmeasured []string
	for name := range f.want {
		if _, ok := f.got[name]; !ok {
			unmeasured = append(unmeasured, name)
		}
	}
	slices.Sort(unmeasured)

	if f.update {
		pins := maps.Clone(f.want)
		maps.Copy(pins, f.got)
		data, err := json.MarshalIndent(pins, "", "  ")
		if err != nil {
			return nil, err
		}

		if err := os.WriteFile(f.path, append(data, '\n'), 0o644); err != nil {
			return nil, err
		}
	}

	return unmeasured, nil
}

// SHA256 is the hex SHA-256 of data.
func SHA256(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}
