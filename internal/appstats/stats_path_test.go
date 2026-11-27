package appstats

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatsFileIsNamedAfterTheRecordingOfAnyFormat(t *testing.T) {
	dir := t.TempDir()
	writer := NewStatsFileWriter(dir, 0o600)

	for recording, stats := range map[string]string{
		"camera-123.webm": "camera-123-stats.json",
		"camera-456.mkv":  "camera-456-stats.json",
	} {
		require.NoError(t, writer.WriteStats(filepath.Join(dir, recording), &StatsFileOutput{}))
		_, err := os.Stat(filepath.Join(dir, stats))
		assert.NoError(t, err, recording)
	}
}
