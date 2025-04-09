package app

import (
	"fmt"
	"os"
	"path"
	"runtime"
	"strings"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal"
	log "github.com/sirupsen/logrus"
)

func configureLog() {
	log.SetOutput(os.Stdout)

	if isTty() {
		log.SetFormatter(&log.TextFormatter{
			CallerPrettyfier: func(f *runtime.Frame) (string, string) {
				filename := path.Base(f.File)
				return fmt.Sprintf("%s:%d", filename, f.Line),
					fmt.Sprintf("> %s()", strings.Replace(f.Function, internal.ModName, ".", 1))
			},
			FullTimestamp: true,
		})
	} else {
		log.SetFormatter(&log.JSONFormatter{CallerPrettyfier: func(f *runtime.Frame) (string, string) {
			return fmt.Sprintf("%s()", strings.Replace(f.Function, internal.ModName, ".", 1)),
				fmt.Sprintf("%s:%d", f.File, f.Line)
		}})
	}

	level, err := log.ParseLevel(cfg.Log.Level)

	if err != nil {
		level = log.InfoLevel
		log.Warnf("Invalid log level '%s', defaulting to INFO", cfg.Log.Level)
	}

	log.SetLevel(level)
	log.SetReportCaller(level >= log.InfoLevel)
}

func isTty() bool {
	if fileInfo, _ := os.Stdout.Stat(); (fileInfo.Mode() & os.ModeCharDevice) != 0 {
		return true
	}
	return false
}
