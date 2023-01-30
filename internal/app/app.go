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
	app struct {
		Name     string
		Version  string
		GitHash  string
		LongName string
	}

	flags struct {
		debug   bool
		config  string
		help    bool
		version bool
	}

	cfg *config.Config
)

func init() {
	app.Name = internal.AppName
	app.Version = internal.AppVersion
	app.LongName = fmt.Sprintf("%s %s", app.Name, app.Version)

	flag.BoolVarP(&flags.help, "help", "h", flags.help, "print help")
	flag.BoolVarP(&flags.debug, "debug", "d", flags.debug, "enable debug log")
	flag.StringVarP(&flags.config, "config", "c", flags.config, "load configuration file")
	flag.Parse()

	if flags.help {
		fmt.Printf("%s\n\n", app.LongName)
		flag.PrintDefaults()
		os.Exit(0)
	}

	if flags.version {
		//fmt.Println(app.LongName)
		os.Exit(0)
	}

	Init()
	Run()
}

func Init() {
	cfg = initConfig()
	log.Infof("Starting %s PID: %d", app.Name, os.Getpid())
	configureLog()
	loadConfig()
	sigintHandler()
	sighupHandler()
}

func Run() {

	go http()
	ps := pubsub.NewPubSub(cfg.PubSub)
	s := server.NewServer(cfg, ps)
	ps.Subscribe(cfg.PubSub.Channel, s.HandlePubSub)
}

func initConfig() *config.Config {
	return (&config.Config{Debug: flags.debug}).GetDefaults(app.Name)
}

func loadConfig() {
	newCfg := initConfig()
	newCfg.Load(app.Name, flags.config)
	*cfg = *newCfg
	configureLog()
}

func sighupHandler() {
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-sighup:
				log.Debug("Reloading config...")
				loadConfig()
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
