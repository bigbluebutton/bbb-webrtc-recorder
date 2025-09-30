package server

import (
	"errors"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/interfaces"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/signal"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/utils"
	pwebrtc "github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
)

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
	statsWriter         *appstats.StatsFileWriter
	startedSuccessfully bool

	mu                    sync.Mutex
	startEvent            *events.StartRecording
	recordingStartTimeUTC time.Time
	recordingStartTimeHR  time.Duration
	mediaHasFlowed        bool
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

func (s *Session) StartRecording(e *events.StartRecording, startTime time.Time) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// A panic here likely means the channel has been closed, which indicates
			// the session is already stopped or in the process of stopping. Start
			// should always fail when facing an error condition (differently from stop).
			log.WithField("session", s.id).Errorf("recovered from panic in start command: %v", r)
			err = errors.New("failed to start session, panic")
		}
	}()

	select {
	case s.commands <- startRecordingCommand{event: e, startTime: startTime}:
		return nil

	default:
		return errors.New("session command queue is full")
	}
}

func (s *Session) StopRecording(e *events.StopRecording, reason string, startTime time.Time) (err error) {
	defer func() {
		if r := recover(); r != nil {
			// A panic here likely means the channel has been closed, which indicates
			// the session is already stopped or in the process of stopping.
			// Stop should fail silently to API consumers. This is our job to get right.
			// Log and register metrics as panics should not happen and should be investigated.
			log.WithField("session", s.id).Errorf("recovered from panic in stop command: %v", r)
			appstats.OnSessionError("panic in stop command")
			err = nil
		}
	}()

	select {
	case s.commands <- stopRecordingCommand{event: e, reason: reason, startTime: startTime}:
		return nil

	default:
		return errors.New("session command queue is full")
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

	return info
}

func (s *Session) Run(wg *sync.WaitGroup) {
	defer wg.Done()
	appstats.Sessions.Inc()
	defer appstats.Sessions.Dec()

	for cmd := range s.commands {
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

	if s.livekit != nil {
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

		if s.livekit != nil {
			// Reset state callbacks to avoid any potential race conditions or duplicated events.
			s.livekit.SetConnectionStateCallback(func(state utils.ConnectionState) {})
			s.livekit.SetFlowCallback(func(isFlowing bool, timestamp time.Duration, closed bool) {})
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

			duration = s.livekit.Close()
		}

		if s.webrtc != nil {
			// Reset state callbacks to avoid any potential race conditions or duplicated events.
			s.webrtc.SetConnectionStateCallback(func(state utils.ConnectionState) {})
			s.webrtc.SetFlowCallback(func(isFlowing bool, videoTimestamp time.Duration, closed bool) {})
			duration = s.webrtc.Close()
		}

		ts := duration / time.Millisecond
		var response interface{}

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

		if s.startedSuccessfully {
			s.server.PublishPubSub(response)
		}

		s.server.CloseSession(s.id)
		close(s.commands)
	})
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
