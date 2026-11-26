package appstats

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	log "github.com/sirupsen/logrus"
)

type StatsFileOutput struct {
	CaptureStats   *CaptureStats `json:"captureStats"`
	StatsTimestamp int64         `json:"statsTimestamp"`
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

func (w *StatsFileWriter) WriteStats(webmFilePath string, stats *StatsFileOutput) error {
	statsFilePath := strings.TrimSuffix(webmFilePath, filepath.Ext(webmFilePath)) + "-stats.json"

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
