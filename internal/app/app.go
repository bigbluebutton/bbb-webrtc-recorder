package app

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/server"
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
)

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
	sigintHandler()
	sighupHandler()
}

func Run() {
	appstats.Init()

	if cfg.Prometheus.Enable {
		appstats.RegisterMetrics()
		appstats.ServePromMetrics(cfg.Prometheus)
	}

	ps = pubsub.NewPubSub(cfg.PubSub)

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

	sv = server.NewServer(cfg, ps)

	if err := ps.Subscribe(cfg.PubSub.Channels.Subscribe, sv.HandlePubSubMsg, sv.OnStart); err != nil {
		log.Fatalf("failed to subscribe to pubsub %s: %s", cfg.PubSub.Channels.Subscribe, err)
	}
}

func shutdown(code int) {
	if ps != nil {
		if err := ps.Close(); err != nil {
			log.Errorf("failed to close pubsub: %s", err)
		}
	}

	if sv != nil {
		if err := sv.Close(); err != nil {
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

func sigintHandler() {
	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt)
	go func() {
		<-sigint
		shutdown(0)
	}()
}
