package pubsub

import (
	"context"
	"fmt"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/mitchellh/mapstructure"
	log "github.com/sirupsen/logrus"
)

type PubSub interface {
	Subscribe(channel string, handler PubSubHandler, onStart func() error)
	Publish(channel string, message []byte)
}

type PubSubHandler func(ctx context.Context, message []byte)

func NewPubSub(cfg config.PubSub) PubSub {
	var err error
	var ps PubSub
	switch cfg.Adapter {
	case "redis":
		c := config.Redis{}
		if err = mapstructure.Decode(cfg.Adapters[cfg.Adapter], &c); err != nil {
			break
		}

		ps = NewRedis(c)
	default:
		err = fmt.Errorf("unknown pubsub adapter '%s'", cfg.Adapter)
	}
	if err != nil {
		log.Fatalf("failed to decode %s pubsub configuration: %s", cfg.Adapter, err)
		return nil
	}
	return ps
}
