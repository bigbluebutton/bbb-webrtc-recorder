package utils

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/livekit"
	log "github.com/sirupsen/logrus"
)

type Stats struct {
	MediaAdapter *livekit.MediaAdapterStats `json:"mediaAdapter"`
	Timestamp    int64                      `json:"timestamp"`
}

type StatsFileWriter struct {
	basePath string
	fileMode os.FileMode
}

func NewStatsFileWriter(basePath string, fileMode os.FileMode) *StatsFileWriter {
	return &StatsFileWriter{
		basePath: basePath,
		fileMode: fileMode,
	}
}

func (w *StatsFileWriter) WriteStats(webmFilePath string, stats *Stats) error {
	statsFilePath := fmt.Sprintf("%s-stats.json", webmFilePath[:len(webmFilePath)-5])

	jsonData, err := json.MarshalIndent(stats, "", "  ")

	if err != nil {
		return fmt.Errorf("JSON marshalling failed: %w", err)
	}

	if err := os.WriteFile(statsFilePath, jsonData, w.fileMode); err != nil {
		return fmt.Errorf("failed to write stats file: %w", err)
	}

	log.WithField("path", statsFilePath).
		WithField("stats", string(jsonData)).
		Tracef("Wrote recording stats to file")

	return nil
}
