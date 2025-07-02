package recorder

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/interfaces"
	"github.com/pion/rtp"
	log "github.com/sirupsen/logrus"
)

// KeyframeRequester defines the interface for requesting keyframes
type KeyframeRequester = interfaces.KeyframeRequester

type Recorder interface {
	GetFilePath() string
	GetStats() *RecorderStats
	PushVideo(rtp *rtp.Packet)
	PushAudio(rtp *rtp.Packet)
	NotifySkippedPacket(seq uint16)
	WithContext(ctx context.Context)
	VideoTimestamp() time.Duration
	AudioTimestamp() time.Duration
	SetHasAudio(hasAudio bool)
	SetHasVideo(hasVideo bool)
	SetKeyframeRequester(requester KeyframeRequester)
	GetHasAudio() bool
	GetHasVideo() bool
	Close() time.Duration
}

func CheckFsPermissions(cfg config.Recorder) error {
	dir := path.Clean(cfg.Directory)

	if err := checkDirectory(dir); err != nil {
		return err
	}

	fileMode, _ := parseFileMode(cfg.FileMode)

	tmpFile, err := os.CreateTemp(dir, ".rec-file-perm-check-*")

	if err != nil {
		return fmt.Errorf("recorder directory is not writable: %w", err)
	}

	defer func() {
		_ = tmpFile.Close()
		if err := os.Remove(tmpFile.Name()); err != nil {
			log.WithField("file", tmpFile.Name()).Warnf("could not remove permission check file: %v", err)
		}
	}()

	// Check if the configured file mode can be applied
	if err := tmpFile.Chmod(fileMode); err != nil {
		return fmt.Errorf("cannot apply file mode %s: %w", cfg.FileMode, err)
	}

	return nil
}

func ValidateAndPrepareFile(ctx context.Context, cfg config.Recorder, file string) (string, os.FileMode, error) {
	dir := path.Clean(cfg.Directory)

	if err := checkDirectory(dir); err != nil {
		return "", 0, err
	}

	if !cfg.WriteToDevNull {
		file = path.Clean(dir + string(os.PathSeparator) + file)
		fileDir := path.Dir(file)

		if _, err := os.Stat(fileDir); err != nil {
			if !os.IsNotExist(err) {
				return "", 0, fmt.Errorf("file directory is not accessible %s", fileDir)
			}

			var dirFileMode os.FileMode

			if parsedFileMode, err := strconv.ParseUint(cfg.DirFileMode, 0, 32); err != nil {
				return "", 0, fmt.Errorf("invalid file mode %s", cfg.DirFileMode)
			} else {
				dirFileMode = os.FileMode(parsedFileMode)
			}

			err = os.MkdirAll(fileDir, dirFileMode)

			if err != nil && !os.IsExist(err) {
				return "", 0, fmt.Errorf("file directory could not be created %s", fileDir)
			}
		}

		if _, err := os.Stat(file); !os.IsNotExist(err) {
			return "", 0, fmt.Errorf("file already exists %s", file)
		}
	} else {
		file = os.DevNull
	}

	var fileMode os.FileMode

	if parsedFileMode, err := strconv.ParseUint(cfg.FileMode, 0, 32); err != nil {
		return "", 0, fmt.Errorf("invalid file mode %s", cfg.FileMode)
	} else {
		fileMode = os.FileMode(parsedFileMode)
	}

	return file, fileMode, nil
}

func NewRecorder(ctx context.Context, cfg config.Recorder, file string) (Recorder, error) {
	ext := filepath.Ext(file)
	file, fileMode, err := ValidateAndPrepareFile(ctx, cfg, file)

	if err != nil {
		return nil, err
	}

	var r Recorder
	switch ext {
	case ".webm":
		r = NewWebmRecorder(
			file,
			fileMode,
			cfg.VideoPacketQueueSize,
			cfg.AudioPacketQueueSize,
			cfg.UseCustomSampler,
			cfg.WriteIVFCopy,
		)
		r.WithContext(ctx)
	default:
		return nil, fmt.Errorf("unsupported file extension %s", ext)
	}
	return r, nil
}

func parseFileMode(mode string) (os.FileMode, error) {
	if parsedFileMode, err := strconv.ParseUint(mode, 0, 32); err != nil {
		return 0, fmt.Errorf("invalid file mode %s", mode)
	} else {
		return os.FileMode(parsedFileMode), nil
	}
}

func checkDirectory(dir string) error {
	if fileInfo, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("recorder directory does not exist: %s", dir)
		}

		if err != nil {
			return fmt.Errorf("could not stat recorder directory %s: %w", dir, err)
		}
	} else {
		if !fileInfo.IsDir() {
			return fmt.Errorf("recorder path is not a directory: %s", dir)
		}
	}

	return nil
}
