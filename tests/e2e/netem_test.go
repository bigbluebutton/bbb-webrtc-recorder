//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The NACK scenario runs livekit-server and redis-server in one network
// namespace and the recorder in another, with netem dropping a share of every
// packet the servers send the recorder. LiveKit's downtrack must repair the
// loss with NACK and RTX.
//
// It needs root and E2E_NETEM=1, and is skipped otherwise, or fails with
// E2E_NETEM_REQUIRED=1. TestNetemPlan and TestNACKRepairControl cover the
// rest without privileges.

const (
	nackLossPct   = 3
	nackCameraFPS = 15
	nackRecordFor = 20 * time.Second

	// Netem dropping a packet and every retransmission of it is rare, 3% of
	// 3% a try. Most unrepaired losses come from LiveKit: a subscriber's PLI
	// stops the downtrack's retransmissions until it forwards a key frame
	// (FlagStopRTXOnPLI), and a PLI within 500 ms of the last one it sent
	// the publisher is not forwarded (the single-layer PLI throttle). A
	// recorder request dropped so leaves every loss unrepaired until the
	// recorder's next request: one outage, however many losses fall in it.
	// Runs show one such outage at most; two leave room.
	nackMaxOutages = 2
	// A loss soon after a request waits up to this long for the next one
	// (lossKeyframeInterval).
	nackRequestInterval = time.Second
	// The track buffer and the writer each ask at most once per second, so
	// a keyframe loop, each key frame's loss asking for the next, asks once
	// a second or more, and unrepaired losses about 0.5 times a run. One per
	// 2 s splits the two.
	nackKeyframeRequestEvery = 2 * time.Second
)

// nackOutageFrames bounds the frames one outage costs: those from its first
// loss to the key frame the next request brings, as the unrepaired-loss
// scenario bounds them at 15 fps, with that request up to nackRequestInterval
// after the loss.
func nackOutageFrames() int {
	sc := lossScenario{fps: nackCameraFPS, request: [2]time.Duration{gapRequestDelay, nackRequestInterval}}

	return int(math.Ceil(nackCameraFPS * sc.lossOutage().Seconds()))
}

// netemPlan names the NACK scenario's two namespaces, their three veth links
// and their addresses, and the commands that build and remove them.
// livekit-server and redis-server run in the server namespace, the recorder in
// the recorder one, and the test process on the host. Every connection that
// crosses into the host namespace is opened from the host, so the host's
// firewall sees only replies to its own traffic.
type netemPlan struct {
	ServerNS   string
	RecorderNS string

	// host <-> server: the test's signalling, Redis and ICE.
	HostSrvIf, SrvHostIf string
	HostSrvIP, SrvHostIP string
	// host <-> recorder: only the test's metrics scrape.
	HostRecIf, RecHostIf string
	HostRecIP, RecHostIP string
	// server <-> recorder: everything the recorder exchanges with the
	// servers. SrvRecIf's egress, server -> recorder, is shaped.
	SrvRecIf, RecSrvIf string
	SrvRecIP, RecSrvIP string

	Loss string // netem's loss argument
	// Dry plans no namespace: everything stays on the host, with loopback
	// addresses in place of the links, for TestNACKRepairControl.
	Dry bool
}

const netemNSPrefix = "bwr-e2e-"

// newNetemPlan derives names and three /30s in 198.18.0.0/15 (RFC 2544's
// benchmarking range, never routed) from pid, so a run's leftovers are
// recognisable and two runs on a host do not collide.
func newNetemPlan(pid int, lossPct int) netemPlan {
	block := 198<<24 + 18<<16 + (pid%(1<<13))*16 // 2^13 /28s fit in the /15; each has a spare /30
	ip := func(off int) string {
		v := block + off
		return fmt.Sprintf("%d.%d.%d.%d", v>>24, v>>16&0xff, v>>8&0xff, v&0xff)
	}

	return netemPlan{
		ServerNS:   fmt.Sprintf("%s%d-srv", netemNSPrefix, pid),
		RecorderNS: fmt.Sprintf("%s%d-rec", netemNSPrefix, pid),
		HostSrvIf:  fmt.Sprintf("bwr%ds", pid%1000000),
		SrvHostIf:  "srv-host",
		HostSrvIP:  ip(1),
		SrvHostIP:  ip(2),
		HostRecIf:  fmt.Sprintf("bwr%dr", pid%1000000),
		RecHostIf:  "rec-host",
		HostRecIP:  ip(5),
		RecHostIP:  ip(6),
		SrvRecIf:   "srv-rec",
		RecSrvIf:   "rec-srv",
		SrvRecIP:   ip(9),
		RecSrvIP:   ip(10),
		Loss:       fmt.Sprintf("%d%%", lossPct),
	}
}

// dryNetemPlan stands loopback addresses in for the links: the servers on
// two (the test's and the recorder's), the recorder's metrics on a third.
// lo has only 127.0.0.1, but Linux routes all of 127/8 to it.
func dryNetemPlan() netemPlan {
	return netemPlan{
		SrvHostIf: "lo", SrvHostIP: "127.0.0.2",
		SrvRecIf: "lo", SrvRecIP: "127.0.0.3",
		RecHostIP: "127.0.0.4",
		Dry:       true,
	}
}

func (p netemPlan) setupCmds() [][]string {
	if p.Dry {
		return nil
	}

	srv, rec := p.ServerNS, p.RecorderNS

	return [][]string{
		{"ip", "netns", "add", srv},
		{"ip", "netns", "add", rec},
		{"ip", "link", "add", p.HostSrvIf, "type", "veth", "peer", "name", p.SrvHostIf, "netns", srv},
		{"ip", "link", "add", p.HostRecIf, "type", "veth", "peer", "name", p.RecHostIf, "netns", rec},
		{"ip", "-n", srv, "link", "add", p.SrvRecIf, "type", "veth", "peer", "name", p.RecSrvIf, "netns", rec},
		{"ip", "addr", "add", p.HostSrvIP + "/30", "dev", p.HostSrvIf},
		{"ip", "link", "set", p.HostSrvIf, "up"},
		{"ip", "addr", "add", p.HostRecIP + "/30", "dev", p.HostRecIf},
		{"ip", "link", "set", p.HostRecIf, "up"},
		{"ip", "-n", srv, "addr", "add", p.SrvHostIP + "/30", "dev", p.SrvHostIf},
		{"ip", "-n", srv, "link", "set", p.SrvHostIf, "up"},
		{"ip", "-n", srv, "addr", "add", p.SrvRecIP + "/30", "dev", p.SrvRecIf},
		{"ip", "-n", srv, "link", "set", p.SrvRecIf, "up"},
		{"ip", "-n", srv, "link", "set", "lo", "up"},
		{"ip", "-n", rec, "addr", "add", p.RecHostIP + "/30", "dev", p.RecHostIf},
		{"ip", "-n", rec, "link", "set", p.RecHostIf, "up"},
		{"ip", "-n", rec, "addr", "add", p.RecSrvIP + "/30", "dev", p.RecSrvIf},
		{"ip", "-n", rec, "link", "set", p.RecSrvIf, "up"},
		{"ip", "-n", rec, "link", "set", "lo", "up"},
		// Everything the servers send the recorder: media, RTX and RTCP, and
		// the signalling and Redis traffic, which TCP repairs. What the
		// recorder sends (its NACKs and PLIs) is not shaped.
		{"tc", "-n", srv, "qdisc", "add", "dev", p.SrvRecIf, "root", "netem", "loss", p.Loss},
	}
}

// teardownCmds removes the namespaces, which takes the veth pairs and the
// qdisc with them; the link deletes cover a namespace that was never made.
func (p netemPlan) teardownCmds() [][]string {
	if p.Dry {
		return nil
	}

	return [][]string{
		{"ip", "netns", "del", p.ServerNS},
		{"ip", "netns", "del", p.RecorderNS},
		{"ip", "link", "del", p.HostSrvIf},
		{"ip", "link", "del", p.HostRecIf},
	}
}

// servers places livekit-server and redis-server on the server namespace's
// two addresses, the test's first, and starts them in that namespace.
func (p netemPlan) servers(t *testing.T) *ServerNet {
	srv := &ServerNet{Addrs: []string{p.SrvHostIP, p.SrvRecIP}}
	if !p.Dry {
		srv.Wrap = func(dir, name, bin string) string { return writeNetnsWrapper(t, p.ServerNS, dir, name, bin) }
	}

	return srv
}

// editLiveKit puts livekit-server's ICE candidates on both of the server
// namespace's links.
func (p netemPlan) editLiveKit(cfg map[string]any) {
	rtc, _ := cfg["rtc"].(map[string]any)
	if rtc == nil {
		rtc = map[string]any{}
		cfg["rtc"] = rtc
	}

	ifs, _ := rtc["interfaces"].(map[string]any)
	if ifs == nil {
		ifs = map[string]any{}
		rtc["interfaces"] = ifs
	}

	includes, _ := ifs["includes"].([]string)
	includes = slices.Clone(includes)

	for _, name := range []string{p.SrvHostIf, p.SrvRecIf} {
		if !slices.Contains(includes, name) {
			includes = append(includes, name)
		}
	}

	ifs["includes"] = includes
}

// editRecorder points the recorder at the servers' address on the
// server <-> recorder link, and moves its metrics endpoint to its address
// on the host link.
func (p netemPlan) editRecorder(t *testing.T, cfg map[string]any, lk *LiveKit, rd *Redis) {
	t.Helper()

	_, redisPort, err := net.SplitHostPort(rd.Addr)
	require.NoError(t, err)

	lookup[map[string]any](t, cfg, "livekit")["host"] = fmt.Sprintf("ws://%s:%d", p.SrvRecIP, lk.HTTPPort)
	lookup[map[string]any](t, cfg, "pubsub", "adapters", "redis")["address"] = net.JoinHostPort(p.SrvRecIP, redisPort)

	prom := lookup[map[string]any](t, cfg, "prometheus")
	_, port, err := net.SplitHostPort(lookup[string](t, cfg, "prometheus", "listenAddress"))
	require.NoError(t, err)
	prom["listenAddress"] = net.JoinHostPort(p.RecHostIP, port)
}

// netnsWrapperScript runs bin inside namespace ns. ip netns exec execs bin
// in its own process, so the child keeps the PID and process group the
// harness signals. Root runs it, so every path is quoted.
func netnsWrapperScript(ipBin, ns, bin string) string {
	return fmt.Sprintf("#!/bin/sh\nexec %s netns exec %s %s \"$@\"\n", shellQuote(ipBin), shellQuote(ns), shellQuote(bin))
}

// shellQuote quotes s as one sh word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// netemSkipReason says why the privileged path cannot run here, or "". The
// gate is euid 0: CAP_NET_ADMIN alone does not let ip netns add mount the
// namespace under /run/netns.
func netemSkipReason() string {
	if os.Geteuid() != 0 || os.Getenv("E2E_NETEM") != "1" {
		return "The NACK scenario needs root and E2E_NETEM=1: it creates two network namespaces, three veth pairs and a netem qdisc. " +
			"tests/e2e/README.md shows how to build as the user and run -test.run '^TestNACKRepairUnderLoss$' as root."
	}

	return ""
}

// TestNACKRepairUnderLoss records a camera for 20 s while netem drops 3% of
// what livekit-server sends the recorder, and checks that LiveKit repaired
// it: lost packets read later, nearly every frame written, no keyframe loop.
func TestNACKRepairUnderLoss(t *testing.T) {
	if reason := netemSkipReason(); reason != "" {
		// CI sets it on the privileged step, where a skip would pass unseen.
		if os.Getenv("E2E_NETEM_REQUIRED") == "1" {
			require.FailNow(t, reason)
		}

		t.Skip(reason)
	}

	plan := newNetemPlan(os.Getpid(), nackLossPct)
	setupNetem(t, plan)

	res := runNACKScenario(t, plan, nackRecordFor)
	loss := readTraceLoss(res.out.Trace)

	t.Logf("netem: %s", res.qdisc)
	t.Logf("trace: %d gaps, %d packets read later, %d gaps never repaired", len(loss.Gaps),
		loss.RepairedPackets(), len(loss.Unrepaired()))

	assert.Positive(t, res.netemDropped, "netem dropped nothing")
	// Counted by sequence number, so a retransmission counts whether
	// LiveKit sends it on the RTX SSRC or on the media one.
	assert.Positive(t, loss.RepairedPackets(), "no lost packet was read later: nothing repaired")
}

// TestNACKRepairControl runs the NACK scenario's stack and checks without
// namespaces and the loss: both servers on two loopback addresses, the test on
// the first and the recorder on the second, the recorder's metrics on a third,
// and the same written-frame and keyframe-request bounds on a clean path.
// LiveKit gets the loopback ICE config here (--node-ip 127.0.0.1), so only the
// NACK scenario covers its namespace ICE config, and only it can show that the
// recorder's ICE pair uses the server <-> recorder link.
func TestNACKRepairControl(t *testing.T) {
	runNACKScenario(t, dryNetemPlan(), 8*time.Second)
}

// TestNetemPlan checks the namespace plan and the config edits the NACK
// scenario makes.
func TestNetemPlan(t *testing.T) {
	p := newNetemPlan(4194303, nackLossPct) // the largest default pid_max on Linux

	for _, name := range []string{p.HostSrvIf, p.SrvHostIf, p.HostRecIf, p.RecHostIf, p.SrvRecIf, p.RecSrvIf} {
		assert.LessOrEqual(t, len(name), 15, "interface name %q over IFNAMSIZ", name)
	}

	assert.True(t, strings.HasPrefix(p.ServerNS, netemNSPrefix))
	assert.True(t, strings.HasPrefix(p.RecorderNS, netemNSPrefix))
	assert.NotEqual(t, p.ServerNS, p.RecorderNS)
	assert.NotEqual(t, p.HostSrvIf, p.HostRecIf)

	_, bench, _ := net.ParseCIDR("198.18.0.0/15")
	var nets []*net.IPNet

	for _, pair := range [][2]string{{p.HostSrvIP, p.SrvHostIP}, {p.HostRecIP, p.RecHostIP}, {p.SrvRecIP, p.RecSrvIP}} {
		_, link, err := net.ParseCIDR(pair[0] + "/30")
		require.NoError(t, err)

		for _, ip := range pair {
			assert.True(t, bench.Contains(net.ParseIP(ip)), "%s in 198.18.0.0/15", ip)
			assert.True(t, link.Contains(net.ParseIP(ip)), "%s and %s share a /30", pair[0], pair[1])
			// Neither the network nor the broadcast address of the /30.
			assert.NotContains(t, []byte{0, 3}, net.ParseIP(ip).To4()[3]&3, "%s", ip)
		}

		assert.NotEqual(t, pair[0], pair[1])

		for _, other := range nets {
			assert.False(t, other.Contains(link.IP), "%s overlaps %s", link, other)
		}

		nets = append(nets, link)
	}

	assert.NotEqual(t, newNetemPlan(1234, nackLossPct).SrvRecIP, newNetemPlan(1235, nackLossPct).SrvRecIP)

	cmds := p.setupCmds()
	assert.Equal(t, []string{"tc", "-n", p.ServerNS, "qdisc", "add", "dev", p.SrvRecIf, "root", "netem", "loss", "3%"},
		cmds[len(cmds)-1], "loss on the server's end of the recorder link: server -> recorder")

	// The host namespace gets the two namespaces and its ends of their links,
	// nothing else: no forwarding, no route, no firewall rule.
	for _, c := range cmds {
		if slices.Contains(c, "-n") {
			continue
		}

		switch {
		case slices.Equal(c[:3], []string{"ip", "netns", "add"}):
		case c[0] == "ip" && slices.Contains([]string{p.HostSrvIf, p.HostRecIf}, c[3]):
		case c[0] == "ip" && c[1] == "addr" && slices.Contains([]string{p.HostSrvIf, p.HostRecIf}, c[len(c)-1]):
		default:
			assert.Fail(t, "a host-namespace command off the host's link ends", "%v", c)
		}
	}

	assert.Equal(t, [][]string{
		{"ip", "netns", "del", p.ServerNS},
		{"ip", "netns", "del", p.RecorderNS},
		{"ip", "link", "del", p.HostSrvIf},
		{"ip", "link", "del", p.HostRecIf},
	}, p.teardownCmds())
	assert.Empty(t, dryNetemPlan().setupCmds())
	assert.Empty(t, dryNetemPlan().teardownCmds())

	sample := newNetemPlan(123456, nackLossPct)
	for _, c := range sample.setupCmds() {
		t.Logf("setup (pid 123456): %s", strings.Join(c, " "))
	}

	for _, c := range sample.teardownCmds() {
		t.Logf("teardown (pid 123456): %s", strings.Join(c, " "))
	}

	srv := p.servers(t)
	assert.Equal(t, []string{p.SrvHostIP, p.SrvRecIP}, srv.Addrs, "the test reaches the servers on the first")
	assert.NotNil(t, srv.Wrap)
	assert.Nil(t, dryNetemPlan().servers(t).Wrap)

	lk := map[string]any{"rtc": map[string]any{"interfaces": map[string]any{"includes": []string{"lo"}}}}
	p.editLiveKit(lk)
	p.editLiveKit(lk) // idempotent
	assert.Equal(t, []string{"lo", p.SrvHostIf, p.SrvRecIf}, lookup[[]string](t, lk, "rtc", "interfaces", "includes"))

	dryLK := map[string]any{"rtc": map[string]any{"interfaces": map[string]any{"includes": []string{"lo"}}}}
	dryNetemPlan().editLiveKit(dryLK)
	assert.Equal(t, []string{"lo"}, lookup[[]string](t, dryLK, "rtc", "interfaces", "includes"))

	rec := RecorderConfig(t, t.TempDir(), &Redis{Addr: "127.0.0.1:1"}, "to", "from",
		&LiveKit{URL: "ws://127.0.0.1:2", HTTPPort: 2})
	p.editRecorder(t, rec, &LiveKit{HTTPPort: 2}, &Redis{Addr: p.SrvHostIP + ":3"})
	assert.Equal(t, "ws://"+p.SrvRecIP+":2", lookup[string](t, rec, "livekit", "host"))
	assert.Equal(t, p.SrvRecIP+":3", lookup[string](t, rec, "pubsub", "adapters", "redis", "address"))
	assert.True(t, strings.HasPrefix(lookup[string](t, rec, "prometheus", "listenAddress"), p.RecHostIP+":"))

	assert.Equal(t, "#!/bin/sh\nexec '/sbin/ip' netns exec '"+p.RecorderNS+"' '/x/rec' \"$@\"\n",
		netnsWrapperScript("/sbin/ip", p.RecorderNS, "/x/rec"))

	// Paths with a space or a quote reach ip as single arguments. A stand-in
	// for ip prints each argument it gets on a line of its own.
	dir := filepath.Join(t.TempDir(), "with space's")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	fakeIP := filepath.Join(dir, "ip")
	require.NoError(t, os.WriteFile(fakeIP, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755))
	wrapper := filepath.Join(dir, "wrapper.sh")
	require.NoError(t, os.WriteFile(wrapper, []byte(netnsWrapperScript(fakeIP, p.RecorderNS, filepath.Join(dir, "rec"))), 0o755))
	out, err := exec.Command(wrapper, "--config", "a b").Output()
	require.NoError(t, err)
	assert.Equal(t, []string{"netns", "exec", p.RecorderNS, filepath.Join(dir, "rec"), "--config", "a b"},
		strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"))

	for _, name := range []string{p.ServerNS, p.RecorderNS, netemNSPrefix + "4194303"} {
		pid, ok := netemNSPid(name)
		assert.True(t, ok, name)
		assert.Equal(t, 4194303, pid, name)
	}

	_, ok := netemNSPid("bwr-e2e-x-srv")
	assert.False(t, ok)

	m := netemDropped.FindStringSubmatch(" Sent 98 bytes 1 pkt (dropped 7, overlimits 0 requeues 0)")
	require.Len(t, m, 2)
	assert.Equal(t, "7", m[1])
}

// nackResult is what runNACKScenario measured, for the privileged checks.
type nackResult struct {
	out          *Output
	qdisc        string
	netemDropped int
}

// runNACKScenario records a camera for recordFor through plan's network and
// asserts the bounds that hold with or without loss.
func runNACKScenario(t *testing.T, plan netemPlan, recordFor time.Duration) nackResult {
	t.Helper()

	prevBin := recorderBin
	defer func() { recorderBin = prevBin }()

	s := NewStack(t, StackOptions{
		Servers: plan.servers(t),
		LiveKitConfig: func(cfg map[string]any) {
			plan.editLiveKit(cfg)
			// The selected ICE pairs, which runNACKScenario checks, are logged
			// at debug.
			lookup[map[string]any](t, cfg, "logging")["level"] = "debug"
		},
		RecorderConfig: func(s *Stack, cfg map[string]any) {
			plan.editRecorder(t, cfg, s.LiveKit, s.Redis)

			if !plan.Dry {
				// StartRecorder runs recorderBin; this run's recorder starts in its namespace.
				recorderBin = writeNetnsWrapper(t, plan.RecorderNS, s.Artifacts, "recorder", prevBin)
			}
		},
	})

	recorderBin = prevBin

	room := "e2e-nack-" + uuid.NewString()[:8]
	pub := s.NewPublisher(room, "e2e-publisher")

	video, err := corpus.GenerateVP8(corpus.VP8Options{FPS: nackCameraFPS, Duration: recordFor + 30*time.Second, SpreadUs: 2000})
	require.NoError(t, err)

	cam := pub.PublishVP8(TrackOptions{Name: "camera", Schedule: video})

	ctx, cancel := context.WithTimeout(t.Context(), recordFor+time.Minute)
	defer cancel()

	require.NoError(t, pub.WaitPublished(ctx, cam))

	rec, err := s.Driver.StartRecording(ctx, room, cam.SID)
	require.NoError(t, err)
	started := time.Now()

	_, err = s.Driver.WaitRtpStatus(ctx, rec.From, rec.SessionID, "flowing")

	var pairs []string
	_ = WaitFor(2*time.Second, nil, func() error {
		if pairs = recorderICEPairs(t, s.LiveKit.Proc.LogPath, rec.SessionID); len(pairs) == 0 {
			return errors.New("none")
		}

		return nil
	})
	require.NoError(t, err, "not flowing; the server's selected ICE addresses for the recorder: %v", pairs)
	require.NotEmpty(t, pairs, "no selected ICE pair for the recorder in %s", s.LiveKit.Proc.LogPath)

	if !plan.Dry {
		// Only the server <-> recorder link carries the loss; the recorder's
		// namespace has no route to the servers' other address.
		require.Equal(t, plan.SrvRecIP, pairs[len(pairs)-1], "the recorder's ICE pair, server side (all: %v)", pairs)
	}

	time.Sleep(time.Until(started.Add(recordFor)))

	var res nackResult
	if !plan.Dry {
		res.qdisc, res.netemDropped = readNetemStats(t, plan, s.Artifacts)
	}

	stopSent := time.Now()
	st, err := s.Driver.StopAndWait(ctx, rec.SessionID)
	require.NoError(t, err)
	assert.Equal(t, events.StopReasonNormal, st.Reason)

	out := ReadOutput(t, rec, cam.SID)
	webm := out.WebM
	res.out = out

	// Compared from the first frame written. When netem drops the first
	// packet of the downtrack's first key frame, no NACK can name it, and the
	// recorder asks for another only once the jitter buffer's 2 s expire.
	winStart := stopSent.Add(-time.Duration(webm.LastPTS-webm.FirstPTS) * time.Millisecond)
	sent, _ := cam.SentBetween(winStart.Sub(cam.Start()), stopSent.Sub(cam.Start()))
	written := webm.Blocks

	pliKeys := 0
	for _, p := range cam.Sent() {
		if p.PLIKey && p.Index == 0 && p.At >= started.Sub(cam.Start()) {
			pliKeys++
		}
	}

	requests := 0
	for _, n := range out.PLIs {
		requests += n
	}

	maxRequests := int(recordFor / nackKeyframeRequestEvery)

	minWritten := sent - nackMaxOutages*nackOutageFrames()

	t.Logf("webm: blocks=%d written-in-window=%d of %d sent (%.1f%%, min %d) maxGap=%dms; trace: frames=%d "+
		"packets=%d pad=%d rtx=%d plis=%v; publisher: plis=%d pli-keyframes=%d nacks=%d; ice: %v",
		webm.Blocks, written, sent, 100*float64(written)/float64(max(sent, 1)), minWritten, webm.MaxGapMs,
		out.ReadFrames, out.ReadPackets, out.PadPackets, out.RTXPackets, out.PLIs, len(cam.PLIs()), pliKeys,
		cam.NACKs(), pairs)

	assert.NoError(t, webm.CheckMonotonic())
	assert.NoError(t, webm.CheckNoDuplicatePTS())
	assert.GreaterOrEqual(t, written, minWritten, "frames written of those sent")
	assert.LessOrEqual(t, requests, maxRequests, "recorder keyframe requests: a keyframe loop")
	// LiveKit's own PLI as the downtrack starts, plus one per recorder request at most.
	assert.LessOrEqual(t, pliKeys, maxRequests+1, "key frames sent for PLIs: a keyframe loop")

	if assert.NotNil(t, out.EOF, "trace eof") {
		assert.Empty(t, out.EOF.Reason, "trace truncated or dropped records")
	}

	return res
}

// recorderICEPairs returns the server-side address of each ICE pair that
// livekit-server's debug log shows it selected for the recorder session's
// subscriber PC, in order.
func recorderICEPairs(t *testing.T, logPath, sessionID string) []string {
	t.Helper()

	data, err := os.ReadFile(logPath)
	require.NoError(t, err)

	var out []string

	for _, line := range strings.Split(string(data), "\n") {
		var e struct {
			Msg         string `json:"msg"`
			Participant string `json:"participant"`
			Transport   string `json:"transport"`
			Pair        struct {
				LocalAddress string `json:"localAddress"`
			} `json:"pair"`
		}

		if json.Unmarshal([]byte(line), &e) != nil || e.Msg != "selected ICE candidate pair changed" ||
			e.Participant != "bbb-webrtc-recorder-"+sessionID || e.Transport != "SUBSCRIBER" {
			continue
		}

		out = append(out, e.Pair.LocalAddress)
	}

	return out
}

// setupNetem builds plan's namespaces and links, removing first any a dead
// run left, and removes them when the test ends.
func setupNetem(t *testing.T, plan netemPlan) {
	t.Helper()

	removeStaleNetns(t)

	// Registered before the stack starts, so it runs after the processes in
	// the namespaces have stopped.
	t.Cleanup(func() {
		for _, c := range plan.teardownCmds() {
			_ = exec.Command(c[0], c[1:]...).Run()
		}
	})

	for _, c := range plan.setupCmds() {
		out, err := exec.Command(c[0], c[1:]...).CombinedOutput()
		require.NoError(t, err, "%s: %s (netem needs the sch_netem module: modprobe sch_netem)",
			strings.Join(c, " "), strings.TrimSpace(string(out)))
	}

	t.Logf("netem: servers in netns %s at %s (test) and %s (recorder), recorder in netns %s at %s "+
		"(metrics %s), loss %s on %s's egress",
		plan.ServerNS, plan.SrvHostIP, plan.SrvRecIP, plan.RecorderNS, plan.RecSrvIP, plan.RecHostIP, plan.Loss, plan.SrvRecIf)
}

// removeStaleNetns deletes the namespaces of NACK scenario runs whose process
// is gone, and those runs' host-side links.
func removeStaleNetns(t *testing.T) {
	out, err := exec.Command("ip", "netns", "list").Output()
	if err != nil {
		return
	}

	for _, line := range strings.Split(string(out), "\n") {
		name, _, _ := strings.Cut(strings.TrimSpace(line), " ")

		pid, ok := netemNSPid(name)
		if !ok || !isDead(pid) {
			continue
		}

		t.Logf("netem: removing stale netns %s", name)
		_ = exec.Command("ip", "netns", "del", name).Run()

		stale := newNetemPlan(pid, nackLossPct)
		_ = exec.Command("ip", "link", "del", stale.HostSrvIf).Run()
		_ = exec.Command("ip", "link", "del", stale.HostRecIf).Run()
	}
}

// netemNSPid returns the pid in a NACK scenario namespace's name.
func netemNSPid(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, netemNSPrefix)
	if !ok {
		return 0, false
	}

	digits, _, _ := strings.Cut(rest, "-")
	pid, err := strconv.Atoi(digits)

	return pid, err == nil
}

func isDead(pid int) bool {
	return syscall.Kill(pid, 0) == syscall.ESRCH
}

// writeNetnsWrapper writes dir/<name>-in-netns.sh, which runs bin in ns.
func writeNetnsWrapper(t *testing.T, ns, dir, name, bin string) string {
	t.Helper()

	ipBin, err := exec.LookPath("ip")
	require.NoError(t, err)

	path := filepath.Join(dir, name+"-in-netns.sh")
	require.NoError(t, os.WriteFile(path, []byte(netnsWrapperScript(ipBin, ns, bin)), 0o755))

	return path
}

var netemDropped = regexp.MustCompile(`dropped (\d+)`)

// readNetemStats saves the shaped link's qdisc statistics as an artifact
// and returns them with netem's drop count.
func readNetemStats(t *testing.T, plan netemPlan, dir string) (string, int) {
	t.Helper()

	out, err := exec.Command("tc", "-n", plan.ServerNS, "-s", "qdisc", "show", "dev", plan.SrvRecIf).CombinedOutput()
	require.NoError(t, err, "tc -s qdisc: %s", out)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "netem-qdisc.txt"), out, 0o644))

	m := netemDropped.FindSubmatch(out)
	require.Len(t, m, 2, "no drop count in %q", out)

	n, err := strconv.Atoi(string(m[1]))
	require.NoError(t, err)

	return strings.TrimSpace(string(out)), n
}
