package server

import (
	"errors"
	"os"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/interfaces"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/signal"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/utils"
	pwebrtc "github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
)

func isInterfaceNil(i interface{}) bool {
	if i == nil {
		return true
	}

	v := reflect.ValueOf(i)
	return !v.IsValid() || v.IsNil()
}

type startRecordingCommand struct {
	event     *events.StartRecording
	startTime time.Time
}

type stopRecordingCommand struct {
	event     *events.StopRecording
	reason    string
	startTime time.Time
}

type Session struct {
	id                  string
	server              *Server
	cfg                 *config.Config
	webrtc              *webrtc.WebRTC
	livekit             interfaces.LiveKitWebRTCInterface
	recorder            recorder.Recorder
	stopped             bool
	stoppedOnce         sync.Once
	commands            chan interface{}
	done                chan struct{}
	statsWriter         *appstats.StatsFileWriter
	startedSuccessfully bool

	mu                    sync.Mutex
	startEvent            *events.StartRecording
	recordingStartTimeUTC time.Time
	recordingStartTimeHR  time.Duration
	mediaHasFlowed        bool
	mediaStart            time.Time
	metadata              map[string]any
}

func NewSession(
	id string,
	s *Server,
	wrtc *webrtc.WebRTC,
	lk interfaces.LiveKitWebRTCInterface,
	recorder recorder.Recorder,
) *Session {
	sess := &Session{
		id:       id,
		server:   s,
		webrtc:   wrtc,
		livekit:  lk,
		recorder: recorder,
		cfg:      s.cfg,
		commands: make(chan interface{}, 10),
		done:     make(chan struct{}),
	}

	if s.cfg.Recorder.WriteStatsFile {
		var fileMode os.FileMode

		if parsedFileMode, err := strconv.ParseUint(s.cfg.Recorder.FileMode, 0, 32); err == nil {
			fileMode = os.FileMode(parsedFileMode)
		} else {
			log.WithField("session", id).
				Warnf("Invalid stats file mode %s, using 0600", s.cfg.Recorder.FileMode)
			fileMode = 0600
		}

		sess.statsWriter = appstats.NewStatsFileWriter(s.cfg.Recorder.Directory, fileMode)
	}

	return sess
}

func (s *Session) StartRecording(e *events.StartRecording, startTime time.Time) error {
	if s.isDone() {
		return errors.New("session already stopped")
	}

	select {
	case s.commands <- startRecordingCommand{event: e, startTime: startTime}:
		return nil

	default:
		return errors.New("session command queue is full")
	}
}

func (s *Session) StopRecording(e *events.StopRecording, reason string, startTime time.Time) error {
	// A session that stopped has nothing left to stop.
	if s.isDone() {
		return nil
	}

	select {
	case s.commands <- stopRecordingCommand{event: e, reason: reason, startTime: startTime}:
		return nil

	default:
		return errors.New("session command queue is full")
	}
}

func (s *Session) isDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *Session) GetRecordingInfo() *events.RecordingInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.startEvent == nil || s.recorder == nil {
		return nil
	}

	info := &events.RecordingInfo{
		SessionId:      s.id,
		FileName:       s.recorder.GetFilePath(),
		Adapter:        s.startEvent.Adapter,
		AdapterOptions: s.startEvent.AdapterOptions,
		Metadata:       s.metadata,
	}

	if s.mediaHasFlowed {
		utc := s.recordingStartTimeUTC.UnixMilli()
		hr := s.recordingStartTimeHR
		info.StartTimeUTC = &utc
		info.StartTimeHR = &hr
	}

	if !s.mediaStart.IsZero() {
		info.SetMediaStart(s.mediaStart)
	}

	return info
}

func (s *Session) Run(wg *sync.WaitGroup) {
	defer wg.Done()
	appstats.Sessions.Inc()
	defer appstats.Sessions.Dec()

	// A failed start stops the session from inside handleStartRecording, so
	// done, not the stop command, is what ends the loop. select picks at random
	// when both are ready, hence the second check.
	for {
		var cmd interface{}
		select {
		case <-s.done:
			return
		case cmd = <-s.commands:
		}

		if s.isDone() {
			return
		}

		switch c := cmd.(type) {
		case startRecordingCommand:
			s.handleStartRecording(c)

		case stopRecordingCommand:
			s.handleStopRecording(c)
			return

		default:
			log.WithField("session", s.id).Errorf("unknown command type: %T", c)
			return
		}
	}
}

func (s *Session) handleStartRecording(c startRecordingCommand) {
	e := c.event
	defer func() {
		if e != nil && !c.startTime.IsZero() {
			appstats.ObserveRequestDuration(e.Id, time.Since(c.startTime))
		}
	}()

	s.mu.Lock()
	// Store the start event for GetRecordingInfo requests
	s.startEvent = e
	s.metadata = e.Metadata
	s.mu.Unlock()

	// webrtc is the mediasoup-based adapter
	if s.webrtc != nil {
		offer := pwebrtc.SessionDescription{}
		signal.Decode(e.GetSDP(), &offer)
		s.webrtc.SetConnectionStateCallback(func(state utils.ConnectionState) {
			if state.IsTerminalState() {
				s.StopRecording(nil, state.String(), time.Time{})
			}
		})
		s.webrtc.SetFlowCallback(func(isFlowing bool, timestamp time.Duration, closed bool) {
			s.handleFirstMediaFlow(isFlowing, timestamp)

			if !closed {
				s.server.PublishPubSub(
					events.NewRecordingRtpStatusChanged(s.id, isFlowing, timestamp/time.Millisecond),
				)
			}
		})
		s.webrtc.SetSDPOffer(offer)
		answer, err := s.webrtc.Init()

		if err != nil {
			s.server.PublishPubSub(e.Fail(err))
			appstats.OnSessionError("init_failed")
			s.handleStopRecording(stopRecordingCommand{reason: "init_failed"})
			return
		}

		s.server.PublishPubSub(e.Success(signal.Encode(answer), s.recorder.GetFilePath(), s.metadata))
		s.startedSuccessfully = true

		return
	}

	if !isInterfaceNil(s.livekit) {
		s.livekit.SetConnectionStateCallback(func(state utils.ConnectionState) {
			if state.IsTerminalState() {
				s.StopRecording(nil, state.String(), time.Time{})
			}
		})
		s.livekit.SetFlowCallback(func(isFlowing bool, timestamp time.Duration, closed bool) {
			s.handleFirstMediaFlow(isFlowing, timestamp)

			if !closed {
				s.server.PublishPubSub(
					events.NewRecordingRtpStatusChanged(s.id, isFlowing, timestamp/time.Millisecond),
				)
			} else {
				s.StopRecording(nil, "closed", time.Time{})
			}
		})
		s.livekit.SetMediaStartCallback(s.handleMediaStart)

		if err := s.livekit.Init(); err != nil {
			s.server.PublishPubSub(e.Fail(err))
			appstats.OnSessionError("init_failed")
			s.handleStopRecording(stopRecordingCommand{reason: "init_failed"})

			return
		}

		// For LiveKit, we don't need to return an SDP answer
		s.server.PublishPubSub(e.Success("", s.recorder.GetFilePath(), s.metadata))
		s.startedSuccessfully = true
	}
}

func (s *Session) handleStopRecording(c stopRecordingCommand) {
	s.stoppedOnce.Do(func() {
		defer func() {
			if !c.startTime.IsZero() {
				method := "stopRecording"
				if c.event != nil {
					method = c.event.Id
				}

				appstats.ObserveRequestDuration(method, time.Since(c.startTime))
			}
		}()

		s.stopped = true
		var duration time.Duration
		var span types.MediaSpan
		var hasSpan bool

		if !isInterfaceNil(s.livekit) {
			// Reset state callbacks to avoid any potential race conditions or duplicated events.
			s.livekit.SetConnectionStateCallback(func(state utils.ConnectionState) {})
			s.livekit.SetFlowCallback(func(isFlowing bool, timestamp time.Duration, closed bool) {})
			s.livekit.SetMediaStartCallback(func(start time.Time) {})
			s.livekit.StopConstraints()
			duration = s.livekit.Close()
			span, hasSpan = s.recorder.MediaSpan()

			// Take the stats after Close, so they include the frames written
			// while closing.
			stats := s.livekit.GetStats()
			appstats.UpdateCaptureMetrics(stats)

			// Write detailed stats to file if enabled
			if s.statsWriter != nil {
				fileStats := &appstats.StatsFileOutput{
					CaptureStats:   stats,
					StatsTimestamp: time.Now().Unix(),
				}

				if err := s.statsWriter.WriteStats(s.recorder.GetFilePath(), fileStats); err != nil {
					log.WithError(err).Error("Failed to write recording stats")
				}
			}
		}

		if s.webrtc != nil {
			// Reset state callbacks to avoid any potential race conditions or duplicated events.
			s.webrtc.SetConnectionStateCallback(func(state utils.ConnectionState) {})
			s.webrtc.SetFlowCallback(func(isFlowing bool, videoTimestamp time.Duration, closed bool) {})
			duration = s.webrtc.Close()
		}

		ts := duration / time.Millisecond
		var response *events.RecordingStopped

		e := c.event
		reason := c.reason

		if e != nil {
			response = e.Stopped(reason, ts)
		} else {
			if reason == "" {
				reason = events.StopReasonNormal
			}

			response = events.NewRecordingStopped(s.id, reason, ts)
		}

		if hasSpan {
			response.WithMediaSpan(span)
		}

		if s.startedSuccessfully {
			s.server.PublishPubSub(response)
		}

		s.server.CloseSession(s.id)
		// commands stays open: callbacks and the server may still send on it.
		close(s.done)
	})
}

// applyConstraint reports whether c changed the capture's video quality;
// only LiveKit captures can change it.
func (s *Session) applyConstraint(c types.CaptureConstraint) bool {
	if isInterfaceNil(s.livekit) {
		log.WithField("session", s.id).
			WithField("reason", c.Reason).
			Debug("Capture constraint skipped: adapter is not LiveKit")

		return false
	}

	return s.livekit.ApplyConstraint(c)
}

func (s *Session) liftConstraint(reason string) bool {
	if isInterfaceNil(s.livekit) {
		return false
	}

	return s.livekit.LiftConstraint(reason)
}

func (s *Session) handleMediaStart(start time.Time) {
	s.mu.Lock()
	s.mediaStart = start
	s.mu.Unlock()

	s.server.PublishPubSub(events.NewRecordingMediaStarted(s.id, start))
}

func (s *Session) handleFirstMediaFlow(isFlowing bool, timestamp time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if isFlowing && !s.mediaHasFlowed {
		s.mediaHasFlowed = true
		s.recordingStartTimeUTC = time.Now().UTC()
		s.recordingStartTimeHR = timestamp
	}
}
