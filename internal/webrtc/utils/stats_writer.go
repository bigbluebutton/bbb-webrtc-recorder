package utils

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/livekit"
	log "github.com/sirupsen/logrus"
)

type Stats struct {
	MediaAdapter *livekit.MediaAdapterStats
	Writer       interface{} // TODO
	Timestamp    int64
}

type StatsFileWriter struct {
	basePath string
}

func NewStatsFileWriter(basePath string) *StatsFileWriter {
	return &StatsFileWriter{
		basePath: basePath,
	}
}

func (w *StatsFileWriter) WriteStats(webmFilePath string, stats *Stats) error {
	statsFilePath := fmt.Sprintf("%s-stats.json", webmFilePath[:len(webmFilePath)-5])

	jsonData, err := json.MarshalIndent(stats, "", "  ")

	if err != nil {
		return fmt.Errorf("JSON marshalling failed: %w", err)
	}

	if err := os.WriteFile(statsFilePath, jsonData, 0600); err != nil {
		return fmt.Errorf("failed to write stats file: %w", err)
	}

	log.WithField("path", statsFilePath).Debug("Wrote recording stats to file")

	return nil
}
