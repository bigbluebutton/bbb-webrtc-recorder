package utils

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/livekit"
)

func TestStatsFileWriter(t *testing.T) {
	// Create a temporary directory for testing
	tmpDir, err := os.MkdirTemp("", "stats-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create a test writer
	writer := NewStatsFileWriter(tmpDir, 0600)

	// Test data
	testStats := &Stats{
		MediaAdapter: &livekit.MediaAdapterStats{
			RoomID: "test-room",
			Tracks: map[string]*livekit.TrackStats{
				"track1": {
					ParticipantID: "participant1",
					Source:        "camera",
					Buffer: &livekit.BufferStatsWrapper{
						PacketsPushed:  100,
						PacketsPopped:  50,
						PacketsDropped: 10,
						PaddingPushed:  5,
						SamplesPopped:  20,
					},
					Adapter: &livekit.AdapterTrackStats{
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
		Timestamp: time.Now().Unix(),
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

		var readStats Stats
		if err := json.Unmarshal(content, &readStats); err != nil {
			t.Errorf("Failed to unmarshal stats file: %v", err)
		}

		// Verify content matches
		if readStats.MediaAdapter.RoomID != testStats.MediaAdapter.RoomID {
			t.Errorf("RoomID mismatch: got %s, want %s",
				readStats.MediaAdapter.RoomID, testStats.MediaAdapter.RoomID)
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
