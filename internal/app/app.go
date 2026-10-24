package app

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/diskpressure"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/server"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/livekit"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/coreos/go-systemd/daemon"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	flag "github.com/spf13/pflag"
)

var (
	app config.App

	flags struct {
		config  string
		dump    string
		help    bool
		version bool
	}

	cfg *config.Config
	ps  pubsub.PubSub
	sv  *server.Server
	dp  *diskpressure.Monitor
)

// lifecycleMu orders Run's assignments of ps, sv and dp, made on the main
// goroutine, before shutdown's reads of them on the shutdown signal
// handler's goroutine. What their Close and Stop read must be set before the
// assignment.
var lifecycleMu sync.Mutex

func init() {
	app.Name = internal.AppName
	app.Version = internal.AppVersion
	app.LongName = fmt.Sprintf("%s %s", app.Name, app.Version)
	app.InstanceId = uuid.New().String()

	flag.StringVarP(&flags.config, "config", "c", flags.config, "load configuration file")
	flag.StringVar(&flags.dump, "dump", "", "print config value (e.g. 'recorder.directory')")
	flag.BoolVarP(&flags.help, "help", "h", flags.help, "print help")
	flag.BoolVarP(&flags.version, "version", "v", flags.version, "print version")
	flag.Parse()

	if flags.help {
		fmt.Printf("%s\n\n", app.LongName)
		flag.PrintDefaults()
		shutdown(0)
	}

	if flags.version {
		fmt.Println(app.LongName)
		shutdown(0)
	}

	if flags.dump != "" {
		log.SetLevel(log.FatalLevel)
		cfg = initConfig()
		loadConfig()
		dumpConfig()
	}

	Init()
	Run()
}

func Init() {
	cfg = initConfig()
	log.Infof("Starting %s PID: %d", app.Name, os.Getpid())
	loadConfig()
	configureLog()
	shutdownSignalHandler()
	sighupHandler()
}

func Run() {
	appstats.Init()

	if cfg.Prometheus.Enable {
		appstats.RegisterMetrics()
		appstats.ServePromMetrics(cfg.Prometheus)
	}

	pubSub := pubsub.NewPubSub(cfg.PubSub)
	lifecycleMu.Lock()
	ps = pubSub
	lifecycleMu.Unlock()

	if err := ps.Check(); err != nil {
		log.Fatalf("failed to connect to pubsub: %v", err)
	}

	if err := recorder.CheckFsPermissions(cfg.Recorder); err != nil {
		log.Fatalf("failed to check recorder filesystem permissions: %v", err)
	}

	if cfg.LiveKit.HealthCheck.Enable {
		log.WithField("interval", cfg.LiveKit.HealthCheck.Interval).
			WithField("host", cfg.LiveKit.Host).
			Debug("LiveKit health check enabled")

		if err := livekit.CheckConnectivity(cfg.LiveKit); err != nil {
			appstats.SetComponentHealth("livekit", false)

			if cfg.LiveKit.HealthCheck.AbortBootOnFailure {
				log.Fatalf("failed to connect to LiveKit: %v", err)
			} else {
				log.Warnf("failed to connect to LiveKit: %v", err)
			}
		} else {
			appstats.SetComponentHealth("livekit", true)
		}

		go func() {
			ticker := time.NewTicker(cfg.LiveKit.HealthCheck.Interval)
			defer ticker.Stop()
			for {
				<-ticker.C
				if err := livekit.CheckConnectivity(cfg.LiveKit); err != nil {
					log.Warnf("LiveKit health check failed: %v", err)
					appstats.SetComponentHealth("livekit", false)
				} else {
					log.Trace("LiveKit health check succeeded")
					appstats.SetComponentHealth("livekit", true)
				}
			}
		}()
	}

	if _, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
		log.Warnf("failed to notify readiness to systemd: %v", err)
	}

	if cfg.HTTP.Enable {
		hs := server.NewHTTPServer(cfg, ps)
		hs.Serve()
	}

	srv := server.NewServer(cfg, ps)
	lifecycleMu.Lock()
	sv = srv
	lifecycleMu.Unlock()
	startDiskPressureGuard()

	if err := ps.Subscribe(cfg.PubSub.Channels.Subscribe, sv.HandlePubSubMsg, sv.OnStart); err != nil {
		log.Fatalf("failed to subscribe to pubsub %s: %s", cfg.PubSub.Channels.Subscribe, err)
	}
}

func startDiskPressureGuard() {
	dpCfg := cfg.Constraints.DiskPressure

	if !dpCfg.Enable {
		log.Debug("Disk pressure guard disabled")

		return
	}

	for _, issue := range dpCfg.Normalize(cfg.Recorder.Directory) {
		logger := log.WithFields(log.Fields{
			"field": "constraints.diskPressure." + issue.Field,
			"given": issue.Given,
			"used":  issue.Used,
		})

		if issue.Severe {
			logger.Error(issue.Message)
		} else {
			logger.Warn(issue.Message)
		}
	}

	if !dpCfg.Enable {
		return
	}

	constraint := types.CaptureConstraint{
		Reason:       types.ConstraintReasonDiskPressure,
		VideoQuality: dpCfg.Quality(),
		Sources:      dpCfg.TrackSources(),
	}

	monitor := diskpressure.NewMonitor(dpCfg, diskpressure.Statfs)
	monitor.OnChange(func(active bool, _ diskpressure.Sample) {
		if active {
			sv.SetConstraint(constraint)
		} else {
			sv.ClearConstraint(constraint.Reason)
		}
	})
	monitor.Start()
	lifecycleMu.Lock()
	dp = monitor
	lifecycleMu.Unlock()
}

func shutdown(code int) {
	lifecycleMu.Lock()
	monitor, pubSub, srv := dp, ps, sv
	lifecycleMu.Unlock()

	if monitor != nil {
		monitor.Stop()
	}

	if pubSub != nil {
		if err := pubSub.Close(); err != nil {
			log.Errorf("failed to close pubsub: %s", err)
		}
	}

	if srv != nil {
		if err := srv.Close(); err != nil {
			log.Errorf("failed to close server: %s", err)
		}
	}

	os.Exit(code)
}

func sighupHandler() {
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-sighup:
				log.Debug("reloading config...")
				loadConfig()
				configureLog()
			}
		}
	}()
}

// shutdownSignalHandler stops the recorder gracefully on SIGINT and on SIGTERM.
// SIGTERM is what systemd, Docker and Kubernetes send to stop a service; left
// to its default action it kills the process before the sessions close their
// recordings.
func shutdownSignalHandler() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		shutdown(0)
	}()
}
