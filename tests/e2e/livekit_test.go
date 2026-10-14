//go:build e2e && linux

package e2e

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// livekit-server's well-known dev key pair; this server is reachable only
// from this host.
const (
	devAPIKey    = "devkey"
	devAPISecret = "secret"
)

// LiveKit is a livekit-server on free ports, bound to loopback unless its
// stack's ServerNet says otherwise.
type LiveKit struct {
	Host       string // the address the test reaches it on
	URL        string // ws://<Host>:<HTTPPort>
	HTTPPort   int
	TCPPort    int // ICE/TCP
	UDPPort    int // ICE/UDP, one port muxing every peer connection
	APIKey     string
	APISecret  string
	ConfigPath string
	Proc       *Proc
}

// StartLiveKit starts LIVEKIT_SERVER_BIN where srv places it (nil: loopback)
// with a generated config, which edit (if not nil) may change before it is
// written, and waits until it serves.
func StartLiveKit(t *testing.T, dir string, srv *ServerNet, edit func(cfg map[string]any)) *LiveKit {
	t.Helper()

	if srv == nil {
		srv = &loopbackServers
	}

	var lk *LiveKit

	StartRetryingPorts(t, func() (*Proc, error) {
		lk = startLiveKit(t, dir, srv, edit)

		return lk.Proc, WaitFor(20*time.Second, lk.Proc, func() error { return httpOK(lk.HTTPURL() + "/") })
	})

	return lk
}

func startLiveKit(t *testing.T, dir string, srv *ServerNet, edit func(cfg map[string]any)) *LiveKit {
	t.Helper()

	lk := &LiveKit{
		Host:      srv.host(),
		HTTPPort:  FreePort(t, "tcp"),
		TCPPort:   FreePort(t, "tcp"),
		UDPPort:   FreePort(t, "udp"),
		APIKey:    devAPIKey,
		APISecret: devAPISecret,
	}
	lk.URL = fmt.Sprintf("ws://%s:%d", lk.Host, lk.HTTPPort)

	cfg := map[string]any{
		"port":           lk.HTTPPort,
		"bind_addresses": slices.Clone(srv.Addrs),
		"rtc": map[string]any{
			"udp_port":                  lk.UDPPort,
			"tcp_port":                  lk.TCPPort,
			"use_external_ip":           false,
			"enable_loopback_candidate": true,
			// The server's candidates are on lo only. A client may pair them
			// with any of its host addresses, which still keeps media on the host.
			"interfaces": map[string]any{"includes": []string{"lo"}},
			// Handed to clients in place of LiveKit's public defaults, so no
			// STUN request leaves the host: a loopback port nothing listens on.
			"stun_servers": []string{fmt.Sprintf("127.0.0.1:%d", FreePort(t, "udp"))},
		},
		"keys":    map[string]string{lk.APIKey: lk.APISecret},
		"logging": map[string]any{"level": envOr("E2E_LIVEKIT_LOG_LEVEL", "info"), "json": true},
		"turn":    map[string]any{"enabled": false},
	}
	if srv.Wrap == nil {
		// One mux socket: --node-ip advertises every lo address as 127.0.0.1,
		// and only the first of them is registered to answer client checks.
		cfg["rtc"].(map[string]any)["ips"] = map[string]any{"includes": []string{"127.0.0.1/32"}}
	}
	if edit != nil {
		edit(cfg)
	}

	lk.ConfigPath = filepath.Join(dir, "livekit-server.yaml")
	writeYAML(t, lk.ConfigPath, cfg)

	args := []string{"--config", lk.ConfigPath}
	// --node-ip rewrites every host candidate to it: right for one loopback,
	// wrong for a namespace each client reaches over a different link.
	if srv.Wrap == nil {
		args = append(args, "--node-ip", "127.0.0.1")
	}

	bin := srv.command(dir, "livekit-server", envOr("LIVEKIT_SERVER_BIN", "livekit-server"))
	lk.Proc = StartProc(t, "livekit-server", filepath.Join(dir, "livekit-server.log"), dir, childEnv(), bin, args...)

	return lk
}

// HTTPURL is the server's HTTP base URL, for its API.
func (lk *LiveKit) HTTPURL() string {
	return fmt.Sprintf("http://%s:%d", lk.Host, lk.HTTPPort)
}

// Token returns a join token for identity in room, with the default grants
// (publish and subscribe) unless grant is given.
func (lk *LiveKit) Token(t *testing.T, room, identity string, grant *auth.VideoGrant) string {
	t.Helper()

	if grant == nil {
		grant = &auth.VideoGrant{}
	}

	grant.RoomJoin = true
	grant.Room = room

	token, err := auth.NewAccessToken(lk.APIKey, lk.APISecret).
		SetVideoGrant(grant).
		SetIdentity(identity).
		SetValidFor(time.Hour).
		ToJWT()
	require.NoError(t, err)

	return token
}

func httpOK(url string) error {
	c := http.Client{Timeout: 2 * time.Second}

	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck

	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	return nil
}

func writeYAML(t *testing.T, path string, v any) {
	t.Helper()

	data, err := yaml.Marshal(v)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}
