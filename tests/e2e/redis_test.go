//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/gomodule/redigo/redis"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Redis is the redis-server a stack's recorder and driver talk through.
type Redis struct {
	Addr string
	Proc *Proc // nil when REDIS_ADDR names an external server
}

// StartRedis starts redis-server (REDIS_SERVER_BIN) on a free loopback port
// with persistence off, or uses REDIS_ADDR when it is set.
func StartRedis(t *testing.T, dir string) *Redis {
	t.Helper()

	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		require.NoError(t, WaitFor(10*time.Second, nil, func() error { return redisPing(addr) }), "REDIS_ADDR %s", addr)

		return &Redis{Addr: addr}
	}

	bin := envOr("REDIS_SERVER_BIN", "redis-server")
	rd := &Redis{}

	StartRetryingPorts(t, func() (*Proc, error) {
		port := FreePort(t, "tcp")
		rd.Addr = fmt.Sprintf("127.0.0.1:%d", port)

		rd.Proc = StartProc(t, "redis-server", filepath.Join(dir, "redis-server.log"), dir, childEnv(), bin,
			"--port", strconv.Itoa(port),
			"--bind", "127.0.0.1",
			"--save", "",
			"--appendonly", "no",
			"--dir", dir,
			"--daemonize", "no",
		)

		return rd.Proc, WaitFor(10*time.Second, rd.Proc, func() error { return redisPing(rd.Addr) })
	})

	return rd
}

func redisPing(addr string) error {
	c, err := redis.Dial("tcp", addr, redis.DialConnectTimeout(time.Second))
	if err != nil {
		return err
	}
	defer c.Close() //nolint:errcheck

	_, err = c.Do("PING")

	return err
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}

	return def
}

// Message is one pubsub message the driver sent or received.
type Message struct {
	At        time.Time       `json:"at"`
	Dir       string          `json:"dir"` // "out" to the recorder, "in" from it
	ID        string          `json:"id"`
	SessionID string          `json:"recordingSessionId,omitempty"`
	Raw       json.RawMessage `json:"msg"`
}

// Driver talks to the recorder over its pubsub channels as bbb-webrtc-sfu
// does, and keeps every message it received for later waits.
type Driver struct {
	t            *testing.T
	addr         string
	ToRecorder   string
	FromRecorder string

	psc  redis.PubSubConn
	done chan struct{}

	mu      sync.Mutex
	in      []Message
	changed chan struct{} // closed and replaced on each received message
	log     *os.File
}

// NewDriver subscribes to fromRecorder before it returns, so it sees the
// recorderStatus a recorder started afterwards announces itself with.
func NewDriver(t *testing.T, addr, toRecorder, fromRecorder, logPath string) *Driver {
	t.Helper()

	conn, err := redis.Dial("tcp", addr, redis.DialConnectTimeout(5*time.Second))
	require.NoError(t, err)

	logFile, err := os.Create(logPath)
	require.NoError(t, err)

	d := &Driver{
		t:            t,
		addr:         addr,
		ToRecorder:   toRecorder,
		FromRecorder: fromRecorder,
		psc:          redis.PubSubConn{Conn: conn},
		done:         make(chan struct{}),
		changed:      make(chan struct{}),
		log:          logFile,
	}

	require.NoError(t, d.psc.Subscribe(fromRecorder))

	switch v := d.psc.Receive().(type) {
	case redis.Subscription:
	case error:
		require.NoError(t, v)
	default:
		require.FailNow(t, fmt.Sprintf("driver: unexpected first reply %T", v))
	}

	go d.receive()

	t.Cleanup(d.close)

	return d
}

func (d *Driver) receive() {
	defer close(d.done)

	for {
		switch v := d.psc.Receive().(type) {
		case redis.Message:
			d.record("in", v.Data)
		case redis.Subscription:
			if v.Count == 0 {
				return
			}
		case error:
			return
		}
	}
}

func (d *Driver) record(dir string, data []byte) Message {
	var head struct {
		ID        string `json:"id"`
		SessionID string `json:"recordingSessionId"`
	}
	_ = json.Unmarshal(data, &head)

	m := Message{At: time.Now(), Dir: dir, ID: head.ID, SessionID: head.SessionID, Raw: json.RawMessage(data)}

	d.mu.Lock()
	defer d.mu.Unlock()

	if line, err := json.Marshal(m); err == nil && d.log != nil {
		_, _ = d.log.Write(append(line, '\n'))
	}

	if dir == "in" {
		d.in = append(d.in, m)
		close(d.changed)
		d.changed = make(chan struct{})
	}

	return m
}

func (d *Driver) close() {
	_ = d.psc.Unsubscribe()

	select {
	case <-d.done:
	case <-time.After(5 * time.Second):
	}

	_ = d.psc.Close()

	d.mu.Lock()
	_ = d.log.Close()
	d.log = nil
	d.mu.Unlock()
}

// Publish sends msg, JSON-encoded, to the recorder's channel.
func (d *Driver) Publish(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	c, err := redis.Dial("tcp", d.addr, redis.DialConnectTimeout(5*time.Second))
	if err != nil {
		return err
	}
	defer c.Close() //nolint:errcheck

	d.record("out", data)
	_, err = c.Do("PUBLISH", d.ToRecorder, data)

	return err
}

// Messages returns the messages received so far, in order.
func (d *Driver) Messages() []Message {
	d.mu.Lock()
	defer d.mu.Unlock()

	return append([]Message(nil), d.in...)
}

// WaitMessage returns the first received message at index from or later
// that match accepts, waiting for one until ctx ends, and its index.
func (d *Driver) WaitMessage(ctx context.Context, from int, match func(Message) bool) (Message, int, error) {
	for {
		d.mu.Lock()
		in, changed := d.in, d.changed
		d.mu.Unlock()

		for i := from; i < len(in); i++ {
			if match(in[i]) {
				return in[i], i, nil
			}
		}

		from = max(from, len(in))

		select {
		case <-changed:
		case <-ctx.Done():
			return Message{}, -1, ctx.Err()
		case <-d.done:
			return Message{}, -1, errors.New("driver: subscription closed")
		}
	}
}

func (d *Driver) waitDecode(ctx context.Context, from int, id, sessionID string, v any) (int, error) {
	m, i, err := d.WaitMessage(ctx, from, func(m Message) bool {
		return m.ID == id && (sessionID == "" || m.SessionID == sessionID)
	})
	if err != nil {
		return -1, fmt.Errorf("waiting for %s (session %q): %w", id, sessionID, err)
	}

	return i, json.Unmarshal(m.Raw, v)
}

// WaitRecorderStatus returns the first recorderStatus at index from or later.
func (d *Driver) WaitRecorderStatus(ctx context.Context, from int) (*events.RecorderStatus, error) {
	var s events.RecorderStatus
	if _, err := d.waitDecode(ctx, from, events.RecorderStatusKey, "", &s); err != nil {
		return nil, err
	}

	return &s, nil
}

// StartSpec is a LiveKit startRecording; zero fields take the defaults noted.
type StartSpec struct {
	Room     string
	TrackIDs []string
	// SessionID defaults to a new UUID, as the SFU uses.
	SessionID string
	// FileName, relative to recorder.directory, defaults to
	// recordings/<room>/<first track ID>-<session ID>.webm.
	FileName string
	Metadata map[string]any
}

// Recording is a started recording session.
type Recording struct {
	SessionID string
	FileName  string // as requested
	Path      string // the response's fileName: the recording's absolute path
	Response  events.StartRecordingResponse
	// From is the index of the first message received after the
	// startRecording: where this start's events begin, for the waits.
	From int
}

// StartRecording starts one LiveKit recording session of trackIDs in room.
func (d *Driver) StartRecording(ctx context.Context, room string, trackIDs ...string) (*Recording, error) {
	return d.StartRecordingWith(ctx, StartSpec{Room: room, TrackIDs: trackIDs})
}

// StartRecordingWith sends the startRecording the SFU's LiveKit recorder
// manager sends and waits for its response; a failed status is an error.
func (d *Driver) StartRecordingWith(ctx context.Context, s StartSpec) (*Recording, error) {
	if s.SessionID == "" {
		s.SessionID = uuid.NewString()
	}

	if s.FileName == "" {
		first := "track"
		if len(s.TrackIDs) > 0 {
			first = s.TrackIDs[0]
		}

		s.FileName = fmt.Sprintf("recordings/%s/%s-%s.webm", s.Room, first, s.SessionID)
	}

	req := events.StartRecording{
		Id:        events.StartRecordingKey,
		SessionId: s.SessionID,
		FileName:  s.FileName,
		Adapter:   events.AdapterLiveKit,
		AdapterOptions: &events.AdapterOptions{
			LiveKit: &events.LiveKitConfig{Room: s.Room, TrackIDs: s.TrackIDs},
		},
		Metadata: s.Metadata,
	}

	from := len(d.Messages())
	if err := d.Publish(req); err != nil {
		return nil, err
	}

	rec := &Recording{SessionID: s.SessionID, FileName: s.FileName, From: from}
	if _, err := d.waitDecode(ctx, from, events.StartRecordingResponseKey, s.SessionID, &rec.Response); err != nil {
		return nil, err
	}

	if err := rec.Response.Validate(); err != nil {
		return rec, fmt.Errorf("invalid startRecordingResponse: %w", err)
	}

	if rec.Response.Status != "ok" {
		return rec, fmt.Errorf("startRecording failed: %s", deref(rec.Response.Error))
	}

	rec.Path = deref(rec.Response.FileName)

	return rec, nil
}

// Stop sends stopRecording for sessionID.
func (d *Driver) Stop(sessionID string) error {
	return d.Publish(events.StopRecording{Id: events.StopRecordingKey, SessionId: sessionID})
}

// WaitStopped returns sessionID's first recordingStopped at message index
// from or later, received before or after the call. A session ID the SFU
// reuses for a retry has one per start, so from must follow the stop or
// start it is for (Recording.From, or len(Messages()) before a Stop).
func (d *Driver) WaitStopped(ctx context.Context, from int, sessionID string) (*events.RecordingStopped, error) {
	var s events.RecordingStopped
	if _, err := d.waitDecode(ctx, from, events.RecordingStoppedKey, sessionID, &s); err != nil {
		return nil, err
	}

	return &s, nil
}

// StopAndWait stops sessionID and waits for the recordingStopped that follows.
func (d *Driver) StopAndWait(ctx context.Context, sessionID string) (*events.RecordingStopped, error) {
	from := len(d.Messages())
	if err := d.Stop(sessionID); err != nil {
		return nil, err
	}

	return d.WaitStopped(ctx, from, sessionID)
}

// WaitRtpStatus returns sessionID's first recordingRtpStatusChanged with
// status ("flowing" or "not_flowing") at message index from or later,
// received before or after the call; from is as for WaitStopped.
func (d *Driver) WaitRtpStatus(ctx context.Context, from int, sessionID, status string) (*events.RecordingRtpStatusChanged, error) {
	m, _, err := d.WaitMessage(ctx, from, func(m Message) bool {
		if m.ID != events.RecordingRtpStatusChangedKey || m.SessionID != sessionID {
			return false
		}

		var e events.RecordingRtpStatusChanged

		return json.Unmarshal(m.Raw, &e) == nil && e.Status == status
	})
	if err != nil {
		return nil, fmt.Errorf("waiting for %s %s: %w", sessionID, status, err)
	}

	var e events.RecordingRtpStatusChanged

	return &e, json.Unmarshal(m.Raw, &e)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
