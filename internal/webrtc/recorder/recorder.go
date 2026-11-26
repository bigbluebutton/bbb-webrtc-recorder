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
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/interfaces"
	"github.com/pion/rtp"
	log "github.com/sirupsen/logrus"
)

// KeyframeRequester defines the interface for requesting keyframes
type KeyframeRequester = interfaces.KeyframeRequester

type Recorder interface {
	GetFilePath() string
	GetStats() *types.RecorderStats
	PushVideo(rtp *rtp.Packet)
	PushAudio(rtp *rtp.Packet)
	// PushVideoAt and PushAudioAt push a packet that arrived at at. MediaSpan
	// is timed by these arrivals, and only LiveKit captures use it.
	PushVideoAt(rtp *rtp.Packet, at time.Time)
	PushAudioAt(rtp *rtp.Packet, at time.Time)
	// PushVideoSample pushes the packets of whole frames that a jitter
	// buffer released together. The video of a codec that TakesSamples,
	// such as H.264, goes only here.
	PushVideoSample(s VideoSample)
	// SetVideoCodec and SetFormat set the video codec and the container.
	// They give the file's track entry and extension, so a change fails
	// once the file is open.
	SetVideoCodec(c *Codec) error
	SetFormat(f Format) error
	// MediaSpan returns where the media written so far sits on the wall
	// clock, and false while no frame is written.
	MediaSpan() (types.MediaSpan, bool)
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

// ValidateAndPrepareFile checks the recorder directory and creates file's
// directory under it. file is a name without extension, under the
// directory. It fails when a recording of that name exists in any format.
func ValidateAndPrepareFile(ctx context.Context, cfg config.Recorder, file string) (string, os.FileMode, error) {
	dir := path.Clean(cfg.Directory)

	if err := checkDirectory(dir); err != nil {
		return "", 0, err
	}

	// The extension is added to this path, so a name that cleans to dir
	// itself would put the recording beside dir.
	full := path.Clean(dir + string(os.PathSeparator) + file)
	if rel, err := filepath.Rel(dir, full); err != nil || rel == "." || !filepath.IsLocal(rel) {
		return "", 0, fmt.Errorf("file %q is not under the recorder directory %s", file, dir)
	}

	// path.Clean turns sub/ and sub/. into sub, so such a name would take
	// the file of a recording named sub.
	if _, last := path.Split(file); last == "" || last == "." || last == ".." {
		return "", 0, fmt.Errorf("file %q names a directory", file)
	}

	if !cfg.WriteToDevNull {
		file = full
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

		for _, f := range []Format{FormatWebM, FormatMKV} {
			if _, err := os.Stat(file + f.Extension()); !os.IsNotExist(err) {
				return "", 0, fmt.Errorf("file already exists %s", file+f.Extension())
			}
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

// NewRecorder returns a WebM recorder of file, a name without extension.
// A trailing .webm or .mkv in file is dropped. SetFormat can make the
// recorder Matroska until the file is open.
func NewRecorder(ctx context.Context, cfg config.Recorder, file string) (Recorder, error) {
	_, base := formatOf(file)
	file, fileMode, err := ValidateAndPrepareFile(ctx, cfg, base)

	if err != nil {
		return nil, err
	}

	if file != os.DevNull {
		file += FormatWebM.Extension()
	}

	r := NewWebmRecorder(
		file,
		fileMode,
		cfg.VideoPacketQueueSize,
		cfg.AudioPacketQueueSize,
		cfg.UseCustomSampler,
		cfg.WriteIVFCopy,
	)
	r.WithContext(ctx)

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
