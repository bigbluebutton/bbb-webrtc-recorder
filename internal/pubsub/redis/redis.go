package redis

import (
	"context"
	"time"

	"github.com/gomodule/redigo/redis"
)

// A ping is set to the server with this period to test for the health of
// the connection and server.
const healthCheckPeriod = time.Minute

type PubSub struct {
	network  string
	address  string
	password string
	conn     redis.Conn
	psc      redis.PubSubConn
}

func NewPubSub(network, address string, password string) (*PubSub, error) {
	p := &PubSub{
		network:  network,
		address:  address,
		password: password,
	}
	if conn, err := p.dial(); err != nil {
		return nil, err
	} else {
		p.conn = conn
		defer conn.Close()
	}
	return p, nil
}

func (p *PubSub) dial() (redis.Conn, error) {
	return redis.Dial(p.network, p.address,
		// Read timeout on server should be greater than ping period.
		redis.DialReadTimeout(healthCheckPeriod+10*time.Second),
		redis.DialWriteTimeout(10*time.Second),
		redis.DialPassword(p.password))
}

func (p *PubSub) ListenChannels(ctx context.Context,
	onStart func() error,
	onMessage func(channel string, data []byte) error,
	channels ...string) error {

	c, err := p.dial()
	if err != nil {
		return err
	}
	defer c.Close()

	p.psc = redis.PubSubConn{Conn: c}

	if err := p.psc.Subscribe(redis.Args{}.AddFlat(channels)...); err != nil {
		return err
	}

	done := make(chan error, 1)

	// Start a goroutine to receive notifications from the server.
	go func() {
		for {
			switch n := p.psc.Receive().(type) {
			case error:
				done <- n
				return
			case redis.Message:
				if err := onMessage(n.Channel, n.Data); err != nil {
					done <- err
					return
				}
			case redis.Subscription:
				switch n.Count {
				case len(channels):
					// Notify application when all channels are subscribed.
					if err := onStart(); err != nil {
						done <- err
						return
					}
				case 0:
					// Return from the goroutine when all channels are unsubscribed.
					done <- nil
					return
				}
			}
		}
	}()

	ticker := time.NewTicker(healthCheckPeriod)
	defer ticker.Stop()
loop:
	for {
		select {
		case <-ticker.C:
			// Send ping to test health of connection and server. If
			// corresponding pong is not received, then receive on the
			// connection will timeout and the receive goroutine will exit.
			if err = p.psc.Ping(""); err != nil {
				break loop
			}
		case <-ctx.Done():
			break loop
		case err := <-done:
			// Return error from the receive goroutine.
			return err
		}
	}

	// Signal the receiving goroutine to exit by unsubscribing from all channels.
	if err := p.psc.Unsubscribe(); err != nil {
		return err
	}

	// Wait for goroutine to complete.
	return <-done
}

func (p *PubSub) Check() error {
	c, err := p.dial()

	if err != nil {
		return err
	}

	defer c.Close()

	if _, err = c.Do("PING"); err != nil {
		return err
	}

	return nil
}

func (p *PubSub) Publish(channel string, message []byte) error {
	c, err := p.dial()
	if err != nil {
		return err
	}
	defer c.Close()

	if _, err = c.Do("PUBLISH", channel, message); err != nil {
		return err
	}

	return nil
}

func (p *PubSub) Close() error {
	return p.conn.Close()
}
