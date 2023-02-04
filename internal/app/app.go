package app

import (
	"fmt"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/server"
	log "github.com/sirupsen/logrus"
	flag "github.com/spf13/pflag"
	"os"
	"os/signal"
	"syscall"
)

var (
	app config.App

	flags struct {
		debug   bool
		config  string
		dump    string
		help    bool
		version bool
	}

	cfg *config.Config
)

func init() {
	app.Name = internal.AppName
	app.Version = internal.AppVersion
	app.LongName = fmt.Sprintf("%s %s", app.Name, app.Version)

	flag.StringVarP(&flags.config, "config", "c", flags.config, "load configuration file")
	flag.StringVar(&flags.dump, "dump", "", "print config value (e.g. 'recorder.directory')")
	flag.BoolVarP(&flags.debug, "debug", "d", flags.debug, "enable debug log")
	flag.BoolVarP(&flags.help, "help", "h", flags.help, "print help")
	flag.BoolVarP(&flags.version, "version", "v", flags.version, "print version")
	flag.Parse()

	if flags.help {
		fmt.Printf("%s\n\n", app.LongName)
		flag.PrintDefaults()
		os.Exit(0)
	}

	if flags.version {
		fmt.Println(app.LongName)
		os.Exit(0)
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
	configureLog()
	loadConfig()
	configureLog()
	sigintHandler()
	sighupHandler()
}

func Run() {
	if cfg.HTTP.Enable {
		ps := pubsub.NewPubSub(cfg.PubSub)
		h := server.NewHTTPServer(cfg, ps)
		h.Serve()
	}
	ps := pubsub.NewPubSub(cfg.PubSub)
	s := server.NewServer(cfg, ps)
	ps.Subscribe(cfg.PubSub.Channels.Subscribe, s.HandlePubSub)
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
		os.Exit(0)
	}()
}
