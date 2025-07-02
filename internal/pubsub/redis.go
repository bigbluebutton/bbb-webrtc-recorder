package pubsub

import (
	"context"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/redis"
	"github.com/cenkalti/backoff/v4"
	log "github.com/sirupsen/logrus"
)

var _ PubSub = (*Redis)(nil)

type Redis struct {
	config       config.Redis
	pubsub       *redis.PubSub
	hasConnected bool
	ctx          context.Context
	cancel       context.CancelFunc
}

func (r *Redis) Subscribe(channel string, handler PubSubHandler, onStart func() error) error {
	eb := backoff.NewExponentialBackOff()
	eb.InitialInterval = 1 * time.Second
	eb.MaxInterval = 30 * time.Second
	eb.MaxElapsedTime = 0
	eb.Multiplier = 2
	eb.RandomizationFactor = 0.1
	eb.Reset()

	for {
		log.Infof("Attemping to subscribe to pubsub %s on redis %s", channel, r.config.Address)

		err := r.pubsub.ListenChannels(r.ctx,
			func() error {
				log.Infof("Subscribed to pubsub %s on redis %s", channel, r.config.Address)
				r.hasConnected = true
				eb.Reset()
				appstats.SetComponentHealth("redis", true)

				return onStart()
			},
			func(channel string, message []byte) error {
				handler(r.ctx, message)
				return nil
			},
			channel)
		if err == nil {
			return nil
		}

		if r.ctx.Err() != nil {
			return r.ctx.Err()
		}

		next := eb.NextBackOff()

		if !r.hasConnected {
			log.Errorf("failed to subscribe to pubsub %s on %s: %s", channel, r.config.Address, err)
			return err
		} else {
			appstats.SetComponentHealth("redis", false)
			log.Errorf("failed to subscribe to pubsub %s on %s: %s - retrying in %s", channel, r.config.Address, err, next)
		}

		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case <-time.After(next):
			continue
		}
	}
}

func (r *Redis) Unsubscribe() {
	r.cancel()
}

func (r *Redis) Publish(channel string, message []byte) error {
	return r.pubsub.Publish(channel, message)
}

func (r *Redis) Check() error {
	return r.pubsub.Check()
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
