//go:build e2e && linux

package e2e

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TCPProxy forwards each TCP connection it accepts to a target, and can
// reset them and refuse new ones, as a signalling outage does. The
// signal-drop scenarios put it between the recorder and livekit-server's
// HTTP/WS port.
type TCPProxy struct {
	Addr   string // host:port it listens on
	target string

	mu       sync.Mutex
	ln       net.Listener
	conns    map[*proxyPair]struct{}
	accepted int
	closed   bool
	wg       sync.WaitGroup
	log      *os.File
}

type proxyPair struct {
	client, upstream net.Conn
}

// StartTCPProxy listens on host (a free port) and forwards to target, logging
// accepts, cuts and outages to logPath; t.Cleanup closes it.
func StartTCPProxy(t *testing.T, host, target, logPath string) *TCPProxy {
	t.Helper()

	logFile, err := os.Create(logPath)
	require.NoError(t, err)

	ln := listenBelowEphemeral(t, host)

	p := &TCPProxy{Addr: ln.Addr().String(), target: target, conns: map[*proxyPair]struct{}{}, log: logFile}
	p.serve(ln)
	p.logf("listening on %s, forwarding to %s", p.Addr, target)

	t.Cleanup(p.Close)

	return p
}

// URL is the proxy's address as a ws:// URL, for livekit.host.
func (p *TCPProxy) URL() string { return "ws://" + p.Addr }

// Accepted counts the connections accepted so far.
func (p *TCPProxy) Accepted() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.accepted
}

// Cut resets both legs of every live connection and returns how many there
// were; new connections are still accepted.
func (p *TCPProxy) Cut() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.cutLocked()
}

// Refuse closes the listener, so that new connections are refused, resets
// every live connection, and listens on the same address again after d.
// It returns the number of connections cut, and a channel that receives nil
// once the proxy listens again, or the error that prevented it.
func (p *TCPProxy) Refuse(d time.Duration) (int, <-chan error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// The listener goes first: the SDK redials within a millisecond of the
	// reset, and a dial that still finds it open would resume the session.
	if p.ln != nil {
		_ = p.ln.Close()
		p.ln = nil
	}

	n := p.cutLocked()
	p.logfLocked("refusing for %s", d)

	ch := make(chan error, 1)
	time.AfterFunc(d, func() { ch <- p.relisten() })

	return n, ch
}

func (p *TCPProxy) relisten() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return errors.New("proxy closed")
	}

	ln, err := net.Listen("tcp4", p.Addr)
	if err != nil {
		p.logfLocked("listen again: %v", err)
		return fmt.Errorf("listen again on %s: %w", p.Addr, err)
	}

	p.ln = ln
	p.wg.Add(1)

	go p.acceptLoop(ln)

	p.logfLocked("listening again")

	return nil
}

// Close stops listening, closes every connection and waits for the
// forwarding goroutines.
func (p *TCPProxy) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}

	p.closed = true

	if p.ln != nil {
		_ = p.ln.Close()
		p.ln = nil
	}

	for c := range p.conns {
		_ = c.client.Close()
		_ = c.upstream.Close()
	}
	p.mu.Unlock()

	p.wg.Wait()

	p.mu.Lock()
	p.logfLocked("closed after %d connections", p.accepted)
	_ = p.log.Close()
	p.log = nil
	p.mu.Unlock()
}

func (p *TCPProxy) serve(ln net.Listener) {
	p.mu.Lock()
	p.ln = ln
	p.mu.Unlock()

	p.wg.Add(1)

	go p.acceptLoop(ln)
}

func (p *TCPProxy) acceptLoop(ln net.Listener) {
	defer p.wg.Done()

	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}

		up, err := net.DialTimeout("tcp", p.target, 5*time.Second)
		if err != nil {
			p.logf("dial %s: %v", p.target, err)
			_ = c.Close()

			continue
		}

		pair := &proxyPair{client: c, upstream: up}

		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = c.Close()
			_ = up.Close()

			return
		}

		p.accepted++
		p.conns[pair] = struct{}{}
		p.logfLocked("accepted #%d from %s", p.accepted, c.RemoteAddr())
		p.wg.Add(1)
		p.mu.Unlock()

		go p.forward(pair)
	}
}

func (p *TCPProxy) forward(pair *proxyPair) {
	defer p.wg.Done()

	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}

	go pipe(pair.upstream, pair.client)
	go pipe(pair.client, pair.upstream)

	// Either side ending ends both: a WebSocket has no use for a half-close.
	<-done
	_ = pair.client.Close()
	_ = pair.upstream.Close()
	<-done

	p.mu.Lock()
	delete(p.conns, pair)
	p.mu.Unlock()
}

func (p *TCPProxy) cutLocked() int {
	n := 0

	for c := range p.conns {
		resetConn(c.client)
		resetConn(c.upstream)
		n++
	}

	p.logfLocked("reset %d connections", n)

	return n
}

// resetConn closes c with an RST rather than a FIN, as a dropped path or a
// restarting proxy does.
func resetConn(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}

	_ = c.Close()
}

func (p *TCPProxy) logf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.logfLocked(format, args...)
}

func (p *TCPProxy) logfLocked(format string, args ...any) {
	if p.log != nil {
		_, _ = fmt.Fprintf(p.log, "%s %s\n", time.Now().Format(time.RFC3339Nano), fmt.Sprintf(format, args...))
	}
}

// listenBelowEphemeral listens on a port below the kernel's ephemeral range:
// Refuse closes the listener, and an outgoing connection must not take the
// port before it listens again.
func listenBelowEphemeral(t *testing.T, host string) net.Listener {
	t.Helper()

	low := 32768
	if b, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range"); err == nil {
		if f := strings.Fields(string(b)); len(f) == 2 {
			if n, err := strconv.Atoi(f[0]); err == nil {
				low = n
			}
		}
	}

	for port := low - 1; port > low-2000 && port > 1024; port-- {
		if ln, err := net.Listen("tcp4", net.JoinHostPort(host, strconv.Itoa(port))); err == nil {
			return ln
		}
	}

	require.FailNow(t, "no free port below the ephemeral range", "low end %d", low)

	return nil
}
