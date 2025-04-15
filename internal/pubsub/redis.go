package pubsub

import (
	"context"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/redis"
	log "github.com/sirupsen/logrus"
)

var _ PubSub = (*Redis)(nil)

type Redis struct {
	config config.Redis
	pubsub *redis.PubSub
	ctx    context.Context
	cancel context.CancelFunc
}

func (r *Redis) Subscribe(channel string, handler PubSubHandler, onStart func() error) {
	log.Printf("subscribing to pubsub %s on redis %s", channel, r.config.Address)
	r.pubsub.ListenChannels(r.ctx,
		func() error {
			onStart()
			return nil
		},
		func(channel string, message []byte) error {
			handler(r.ctx, message)
			return nil
		},
		channel)
}

func (r *Redis) Unsubscribe() {
	r.cancel()
}

func (r *Redis) Publish(channel string, message []byte) {
	r.pubsub.Publish(channel, message)
}

func NewRedis(cfg config.Redis) *Redis {
	r := &Redis{config: cfg}
	if p, err := redis.NewPubSub(cfg.Network, cfg.Address, cfg.Password); err != nil {
		log.Fatalf("failed to start redis pubsub: %s", err)
	} else {
		r.ctx, r.cancel = context.WithCancel(context.Background())
		r.pubsub = p
	}
	return r
}

func (r *Redis) Close() error {
	return r.pubsub.Close()
}
