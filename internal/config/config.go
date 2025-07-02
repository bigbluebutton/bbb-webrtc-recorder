package config

import (
	"os"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/pion/webrtc/v3"
	log "github.com/sirupsen/logrus"
)

type App struct {
	Name       string
	Version    string
	GitHash    string
	LongName   string
	InstanceId string
}

type Config struct {
	App        App        `yaml:"-"`
	Recorder   Recorder   `yaml:"recorder,omitempty"`
	PubSub     PubSub     `yaml:"pubsub,omitempty"`
	WebRTC     WebRTC     `yaml:"webrtc,omitempty"`
	HTTP       HTTP       `yaml:"http,omitempty"`
	Prometheus Prometheus `yaml:"prometheus,omitempty"`
	LiveKit    LiveKit    `yaml:"livekit,omitempty"`
	Log        LogConfig  `yaml:"log"`
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

	cfg.Recorder.DirFileMode = "0700"
	cfg.Recorder.FileMode = "0600"
	cfg.Recorder.WriteToDevNull = false
	cfg.Recorder.WriteIVFCopy = false
	cfg.Recorder.WriteStatsFile = false
	cfg.Recorder.VideoPacketQueueSize = 256
	cfg.Recorder.AudioPacketQueueSize = 32
	cfg.Recorder.UseCustomSampler = true
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
	cfg.WebRTC.RTCMinPort = 24577
	cfg.WebRTC.RTCMaxPort = 32768
	cfg.WebRTC.JitterBuffer = 512
	cfg.WebRTC.JitterBufferPktTimeout = 200
	cfg.HTTP = HTTP{
		Enable: false,
		Port:   8080,
	}
	cfg.Prometheus = Prometheus{
		Enable:        false,
		ListenAddress: "127.0.0.1:3200",
	}
	cfg.LiveKit = LiveKit{
		Host:                  "ws://localhost:7880",
		APIKey:                "",
		APISecret:             "",
		PacketReadTimeout:     500 * time.Millisecond,
		PreferredVideoQuality: livekit.VideoQuality_HIGH,
		HealthCheck: HealthCheck{
			Enable:   false,
			Interval: 1 * time.Minute,
		},
	}
}

type Recorder struct {
	Directory            string `yaml:"directory,omitempty"`
	DirFileMode          string `yaml:"dirFileMode,omitempty"`
	FileMode             string `yaml:"fileMode,omitempty"`
	WriteToDevNull       bool   `yaml:"writeToDevNull,omitempty"`
	WriteIVFCopy         bool   `yaml:"writeIVFCopy,omitempty"`
	VideoPacketQueueSize uint16 `yaml:"videoPacketQueueSize,omitempty"`
	AudioPacketQueueSize uint16 `yaml:"audioPacketQueueSize,omitempty"`
	UseCustomSampler     bool   `yaml:"useCustomSampler,omitempty"`
	WriteStatsFile       bool   `yaml:"writeStatsFile,omitempty"`
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
	ICEServers             []webrtc.ICEServer `yaml:"iceServers,omitempty"`
	RTCMinPort             uint16             `yaml:"rtcMinPort,omitempty"`
	RTCMaxPort             uint16             `yaml:"rtcMaxPort,omitempty"`
	JitterBuffer           uint16             `yaml:"jitterBuffer,omitempty"`
	JitterBufferPktTimeout uint16             `yaml:"jitterBufferPktTimeout,omitempty"`
}

type HTTP struct {
	Enable bool `yaml:"enable,omitempty"`
	Port   int  `yaml:"port,omitempty"`
}

type Prometheus struct {
	Enable        bool   `yaml:"enable,omitempty"`
	ListenAddress string `yaml:"listenAddress,omitempty"`
}

type LiveKit struct {
	Host                  string               `yaml:"host,omitempty" mapstructure:"host"`
	APIKey                string               `yaml:"apiKey,omitempty" mapstructure:"api_key"`
	APISecret             string               `yaml:"apiSecret,omitempty" mapstructure:"api_secret"`
	PacketReadTimeout     time.Duration        `yaml:"packetReadTimeout,omitempty" mapstructure:"packet_read_timeout"`
	PreferredVideoQuality livekit.VideoQuality `yaml:"preferredVideoQuality,omitempty" mapstructure:"preferred_video_quality"`
	HealthCheck           HealthCheck          `yaml:"healthCheck,omitempty"`
}

type HealthCheck struct {
	Enable   bool          `yaml:"enable,omitempty"`
	Interval time.Duration `yaml:"interval,omitempty"`
}

type LogConfig struct {
	Level string `yaml:"level"`
}
