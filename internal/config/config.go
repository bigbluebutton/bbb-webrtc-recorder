package config

import (
	"github.com/pion/webrtc/v3"
	log "github.com/sirupsen/logrus"
	"os"
)

type App struct {
	Name     string
	Version  string
	GitHash  string
	LongName string
}

type Config struct {
	App      App      `yaml:"-"`
	Debug    bool     `yaml:"debug"`
	Recorder Recorder `yaml:"recorder,omitempty"`
	PubSub   PubSub   `yaml:"pubsub,omitempty"`
	WebRTC   WebRTC   `yaml:"webrtc,omitempty"`
	HTTP     HTTP     `yaml:"http,omitempty"`
}

func (cfg *Config) GetDefaults() *Config {
	cfg.SetDefaults()
	return cfg
}

// SetDefaults sets the default values
func (cfg *Config) SetDefaults() {
	if cfg.App.Name == "" {
		var err error
		if cfg.App.Name, err = os.Executable(); err != nil {
			log.Error(err)
			cfg.App.Name = "unknown"
		}
	}

	cfg.PubSub.Channels = Channels{
		Subscribe: "to-" + cfg.App.Name,
		Publish:   "from-" + cfg.App.Name,
	}
	cfg.PubSub.Adapter = "redis"
	cfg.PubSub.Adapters = make(map[string]interface{})
	cfg.PubSub.Adapters["redis"] = &Redis{
		Address:  ":6379",
		Network:  "tcp",
		Password: "",
	}
	cfg.WebRTC.ICEServers = append(cfg.WebRTC.ICEServers, webrtc.ICEServer{
		URLs: []string{"stun:stun.l.google.com:19302"},
	})
	cfg.WebRTC.RTCMinPort = 24577
	cfg.WebRTC.RTCMaxPort = 32768
	cfg.WebRTC.JitterBuffer = 512
	cfg.HTTP = HTTP{
		Enable: false,
		Port:   8080,
	}
}

type Recorder struct {
	Directory string `yaml:"directory,omitempty"`
}

type Redis struct {
	Address  string `yaml:"address,omitempty"`
	Network  string `yaml:"network,omitempty"`
	Password string `yaml:"password,omitempty"`
}

type PubSub struct {
	Channels Channels `yaml:"channels,omitempty"`
	Adapter  string   `yaml:"adapter,omitempty"`
	Adapters map[string]interface{}
}

type Channels struct {
	Subscribe string `yaml:"subscribe,omitempty"`
	Publish   string `yaml:"publish,omitempty"`
}

type WebRTC struct {
	ICEServers   []webrtc.ICEServer `yaml:"iceServers,omitempty"`
	RTCMinPort   uint16             `yaml:"rtcMinPort,omitempty"`
	RTCMaxPort   uint16             `yaml:"rtcMaxPort,omitempty"`
	JitterBuffer uint16             `yaml:"jitterBuffer,omitempty"`
}

type HTTP struct {
	Enable bool `yaml:"enable,omitempty"`
	Port   int  `yaml:"port,omitempty"`
}
