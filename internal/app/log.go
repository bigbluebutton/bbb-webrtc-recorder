package app

import (
	"fmt"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal"
	log "github.com/sirupsen/logrus"
	"os"
	"path"
	"runtime"
	"strings"
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

	if flags.debug || cfg.Debug {
		if log.GetLevel() != log.DebugLevel {
			log.SetReportCaller(true)
			log.SetLevel(log.DebugLevel)
			log.Debug("debug log enabled")
		}
	} else {
		if log.GetLevel() == log.DebugLevel {
			log.Debug("debug log disabled")
		}
		log.SetReportCaller(false)
		log.SetLevel(log.InfoLevel)
	}
}

func isTty() bool {
	if fileInfo, _ := os.Stdout.Stat(); (fileInfo.Mode() & os.ModeCharDevice) != 0 {
		return true
	}
	return false
}
