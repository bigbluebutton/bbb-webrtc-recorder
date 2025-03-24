package recorder

import (
	"context"
	"fmt"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/pion/rtp"
	log "github.com/sirupsen/logrus"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"
)

type KeyframeRequester interface {
	RequestKeyframe()
	RequestKeyframeForSSRC(ssrc uint32)
}

type Recorder interface {
	GetFilePath() string
	PushVideo(rtp *rtp.Packet)
	PushAudio(rtp *rtp.Packet)
	NotifySkippedPacket(seq uint16)
	WithContext(ctx context.Context)
	VideoTimestamp() time.Duration
	SetHasAudio(hasAudio bool)
	SetKeyframeRequester(requester KeyframeRequester)
	GetHasAudio() bool
	Close() time.Duration
}

func NewRecorder(ctx context.Context, cfg config.Recorder, file string) (Recorder, error) {
	ext := filepath.Ext(file)
	dir := path.Clean(cfg.Directory)

	if stat, err := os.Stat(dir); os.IsNotExist(err) {
		log.WithField("session", ctx.Value("session")).
			Debug(stat)
		return nil, fmt.Errorf("directory does not exist %s", cfg.Directory)
	}

	// If we are not writing to /dev/null, we need to check if the file exists
	// and if the directory is writable.
	if !cfg.WriteToDevNull {
		file = path.Clean(dir + string(os.PathSeparator) + file)
		fileDir := path.Dir(file)

		if stat, err := os.Stat(fileDir); err != nil {
			log.WithField("session", ctx.Value("session")).
				Debug(stat)

			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("file directory is not accessible %s", fileDir)
			}

			var dirFileMode os.FileMode
			if parsedFileMode, err := strconv.ParseUint(cfg.DirFileMode, 0, 32); err != nil {
				return nil, fmt.Errorf("invalid file mode %s", cfg.DirFileMode)
			} else {
				dirFileMode = os.FileMode(parsedFileMode)
			}

			err = os.MkdirAll(fileDir, dirFileMode)

			if err != nil && !os.IsExist(err) {
				return nil, fmt.Errorf("file directory could not be created %s", fileDir)
			}
		}

		if stat, err := os.Stat(file); !os.IsNotExist(err) {
			log.WithField("session", ctx.Value("session")).
				Debug(stat)
			return nil, fmt.Errorf("file already exists %s", file)
		}
	} else {
		// We're writing to /dev/null - see the recorder.writeToDevNull config
		// (for testing purposes)
		file = os.DevNull
	}

	var fileMode os.FileMode
	if parsedFileMode, err := strconv.ParseUint(cfg.FileMode, 0, 32); err != nil {
		return nil, fmt.Errorf("invalid file mode %s", cfg.FileMode)
	} else {
		fileMode = os.FileMode(parsedFileMode)
	}

	var r Recorder
	var err error
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
		err = fmt.Errorf("unsupported format '%s'", ext)
	}
	return r, err
}
