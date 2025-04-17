package appstats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStatsFileWriter(t *testing.T) {
	// Create a temporary directory for testing
	tmpDir, err := os.MkdirTemp("", "stats-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer func() {
		if err := os.RemoveAll(tmpDir); err != nil {
			t.Fatalf("Failed to remove temp dir: %v", err)
		}
	}()

	// Create a test writer
	writer := NewStatsFileWriter(tmpDir, 0600)

	// Test data
	testStats := &StatsFileOutput{
		CaptureStats: &CaptureStats{
			RoomID:        "test-room",
			ParticipantID: "test-participant",
			Tracks: map[string]*TrackStats{
				"camera": {
					Source: "camera",
					Buffer: &BufferStatsWrapper{
						PacketsPushed:  100,
						PacketsPopped:  50,
						PacketsDropped: 10,
						PaddingPushed:  5,
						SamplesPopped:  20,
					},
					Adapter: &AdapterTrackStats{
						StartTime:   time.Now().Unix(),
						EndTime:     time.Now().Unix() + 1000,
						FirstSeqNum: 1,
						LastSeqNum:  100,
						PLIRequests: 5,
					},
					TrackKind: "video",
					MimeType:  "video/vp8",
				},
			},
		},
		StatsTimestamp: time.Now().Unix(),
	}

	t.Run("WriteStats_Success", func(t *testing.T) {
		webmPath := filepath.Join(tmpDir, "test.webm")
		if err := writer.WriteStats(webmPath, testStats); err != nil {
			t.Errorf("WriteStats failed: %v", err)
		}

		// Verify file was created
		statsPath := webmPath[:len(webmPath)-5] + "-stats.json"
		if _, err := os.Stat(statsPath); os.IsNotExist(err) {
			t.Errorf("Stats file was not created: %v", err)
		}

		// Verify file content
		content, err := os.ReadFile(statsPath)
		if err != nil {
			t.Errorf("Failed to read stats file: %v", err)
		}

		var readStats StatsFileOutput
		if err := json.Unmarshal(content, &readStats); err != nil {
			t.Errorf("Failed to unmarshal stats file: %v", err)
		}

		// Verify content matches
		if readStats.CaptureStats.RoomID != testStats.CaptureStats.RoomID {
			t.Errorf("RoomID mismatch: got %s, want %s",
				readStats.CaptureStats.RoomID, testStats.CaptureStats.RoomID)
		}

		if readStats.CaptureStats.ParticipantID != testStats.CaptureStats.ParticipantID {
			t.Errorf("ParticipantID mismatch: got %s, want %s",
				readStats.CaptureStats.ParticipantID, testStats.CaptureStats.ParticipantID)
		}

		if readStats.CaptureStats.FileName != testStats.CaptureStats.FileName {
			t.Errorf("FileName mismatch: got %s, want %s",
				readStats.CaptureStats.FileName, testStats.CaptureStats.FileName)
		}
	})

	t.Run("WriteStats_InvalidPath", func(t *testing.T) {
		// Test with invalid path
		invalidPath := filepath.Join(tmpDir, "nonexistent", "test.webm")
		err := writer.WriteStats(invalidPath, testStats)
		if err == nil {
			t.Error("Expected error for invalid path, got nil")
		}
	})

	t.Run("WriteStats_ReadOnlyDir", func(t *testing.T) {
		// Create a read-only directory
		roDir := filepath.Join(tmpDir, "readonly")
		if err := os.Mkdir(roDir, 0444); err != nil {
			t.Fatalf("Failed to create read-only dir: %v", err)
		}

		webmPath := filepath.Join(roDir, "test.webm")
		err := writer.WriteStats(webmPath, testStats)
		if err == nil {
			t.Error("Expected error for read-only directory, got nil")
		}
	})
}
