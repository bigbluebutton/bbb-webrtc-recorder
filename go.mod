module github.com/bigbluebutton/bbb-webrtc-recorder

go 1.19

replace github.com/at-wat/ebml-go => ./pkg/ebml-go

require (
	github.com/AlekSi/pointer v1.2.0
	github.com/at-wat/ebml-go v0.16.0
	github.com/crazy-max/gonfig v0.6.0
	github.com/gomodule/redigo v1.8.9
	github.com/kr/pretty v0.1.0
	github.com/mitchellh/mapstructure v1.5.0
	github.com/pion/interceptor v0.1.12
	github.com/pion/rtcp v1.2.10
	github.com/pion/rtp v1.7.13
	github.com/pion/sdp/v3 v3.0.6
	github.com/pion/webrtc/v3 v3.1.56
	github.com/pkg/errors v0.9.1
	github.com/sirupsen/logrus v1.9.0
	github.com/spf13/pflag v1.0.5
	github.com/titanous/json5 v1.0.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/BurntSushi/toml v1.1.0 // indirect
	github.com/google/uuid v1.3.0 // indirect
	github.com/kr/text v0.1.0 // indirect
	github.com/pion/datachannel v1.5.5 // indirect
	github.com/pion/dtls/v2 v2.2.6 // indirect
	github.com/pion/ice/v2 v2.3.1 // indirect
	github.com/pion/logging v0.2.2 // indirect
	github.com/pion/mdns v0.0.7 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/sctp v1.8.6 // indirect
	github.com/pion/srtp/v2 v2.0.12 // indirect
	github.com/pion/stun v0.4.0 // indirect
	github.com/pion/transport/v2 v2.0.2 // indirect
	github.com/pion/turn/v2 v2.1.0 // indirect
	github.com/pion/udp/v2 v2.0.1 // indirect
	golang.org/x/crypto v0.6.0 // indirect
	golang.org/x/net v0.7.0 // indirect
	golang.org/x/sys v0.5.0 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
)
