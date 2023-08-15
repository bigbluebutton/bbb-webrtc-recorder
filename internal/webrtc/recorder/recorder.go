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
	"time"
)

type Recorder interface {
	GetFilePath() string
	PushVideo(rtp *rtp.Packet)
	PushAudio(rtp *rtp.Packet)
	WithContext(ctx context.Context)
	VideoTimestamp() time.Duration
	SetHasAudio(hasAudio bool)
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

	file = path.Clean(dir + string(os.PathSeparator) + file)
	fileDir := path.Dir(file)

	if stat, err := os.Stat(fileDir); err != nil {
		log.WithField("session", ctx.Value("session")).
			Debug(stat)

		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("file directory is not accessible %s", fileDir)
		}

		err = os.MkdirAll(fileDir, 0700)
		if err != nil && !os.IsExist(err) {
			return nil, fmt.Errorf("file directory could not be created %s", fileDir)
		}
	}

	if stat, err := os.Stat(file); !os.IsNotExist(err) {
		log.WithField("session", ctx.Value("session")).
			Debug(stat)
		return nil, fmt.Errorf("file already exists %s", file)
	}

	var r Recorder
	var err error
	switch ext {
	case ".webm":
		r = NewWebmRecorder(file)
		r.WithContext(ctx)
	default:
		err = fmt.Errorf("unsupported format '%s'", ext)
	}
	return r, err
}
