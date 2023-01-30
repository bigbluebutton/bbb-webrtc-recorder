package config

import (
	"github.com/pion/webrtc/v3"
	log "github.com/sirupsen/logrus"
	"os"
)

type Config struct {
	Debug    bool     `yaml:"debug"`
	Recorder Recorder `yaml:"recorder,omitempty"`
	PubSub   PubSub   `yaml:"pubsub,omitempty"`
	WebRTC   WebRTC   `yaml:"webrtc,omitempty"`
	HTTP     HTTP     `yaml:"http,omitempty"`
}

func (cfg *Config) GetDefaults(app string) *Config {
	cfg.SetDefaults(app)
	return cfg
}

// SetDefaults sets the default values
func (cfg *Config) SetDefaults(app string) {
	if app == "" {
		var err error
		if app, err = os.Executable(); err != nil {
			log.Error(err)
			app = "unknown"
		}
	}
	cfg.PubSub.Channel, _ = os.Executable()
	cfg.PubSub.Adapter = "redis"
	cfg.PubSub.Adapters = make(map[string]interface{})
	cfg.PubSub.Adapters["redis"] = &Redis{
		Address: ":6379",
		Network: "tcp",
	}
	cfg.WebRTC.ICEServers = append(cfg.WebRTC.ICEServers, webrtc.ICEServer{
		URLs: []string{"stun:stun.l.google.com:19302"},
	})
	cfg.HTTP = HTTP{
		Enable: false,
		Port:   8080,
	}
}

type Recorder struct {
	Directory string `yaml:"directory,omitempty"`
}

type Redis struct {
	Address string `yaml:"address,omitempty"`
	Network string `yaml:"network,omitempty"`
}

type PubSub struct {
	Channel  string `yaml:"channel,omitempty"`
	Adapter  string `yaml:"adapter,omitempty"`
	Adapters map[string]interface{}
}

type WebRTC struct {
	ICEServers []webrtc.ICEServer `yaml:"iceServers,omitempty"`
}

type HTTP struct {
	Enable bool `yaml:"enable,omitempty"`
	Port   int  `yaml:"port,omitempty"`
}
