package server

import (
	"os"
	"strconv"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/livekit"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/signal"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/utils"
	pwebrtc "github.com/pion/webrtc/v3"
	log "github.com/sirupsen/logrus"
)

type Session struct {
	id          string
	server      *Server
	cfg         *config.Config
	webrtc      *webrtc.WebRTC
	livekit     *livekit.LiveKitWebRTC
	recorder    recorder.Recorder
	stopped     bool
	statsWriter *appstats.StatsFileWriter
}

func NewSession(id string, s *Server, wrtc *webrtc.WebRTC, lk *livekit.LiveKitWebRTC, recorder recorder.Recorder) *Session {
	sess := &Session{
		id:       id,
		server:   s,
		webrtc:   wrtc,
		livekit:  lk,
		recorder: recorder,
		cfg:      s.cfg,
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

func (s *Session) StartRecording(e *events.StartRecording) (string, error) {
	appstats.Sessions.Inc()
	// Only initialize WebRTC if we're using mediasoup
	if s.webrtc != nil {
		offer := pwebrtc.SessionDescription{}
		signal.Decode(e.GetSDP(), &offer)
		s.webrtc.SetConnectionStateCallback(func(state utils.ConnectionState) {
			if state.IsTerminalState() {
				if !s.stopped {
					ts := s.StopRecording() / time.Millisecond
					s.server.PublishPubSub(events.NewRecordingStopped(s.id, state.String(), ts))
					s.server.CloseSession(s.id)
				}
			}
		})
		s.webrtc.SetFlowCallback(func(isFlowing bool, timestamp time.Duration, closed bool) {
			if !closed {
				s.server.PublishPubSub(
					events.NewRecordingRtpStatusChanged(s.id, isFlowing, timestamp/time.Millisecond),
				)
			} else {
				s.server.CloseSession(s.id)
				s.server.PublishPubSub(
					events.NewRecordingStopped(s.id, "closed", timestamp/time.Millisecond),
				)
			}
		})
		s.webrtc.SetSDPOffer(offer)
		answer, err := s.webrtc.Init()

		if err != nil {
			return "", err
		}

		return signal.Encode(answer), nil
	}

	// For LiveKit, we don't need to return an SDP answer
	if s.livekit != nil {
		s.livekit.SetConnectionStateCallback(func(state utils.ConnectionState) {
			if state.IsTerminalState() {
				if !s.stopped {
					ts := s.StopRecording() / time.Millisecond
					s.server.PublishPubSub(events.NewRecordingStopped(s.id, state.String(), ts))
					s.server.CloseSession(s.id)
				}
			}
		})
		s.livekit.SetFlowCallback(func(isFlowing bool, timestamp time.Duration, closed bool) {
			if !closed {
				s.server.PublishPubSub(
					events.NewRecordingRtpStatusChanged(s.id, isFlowing, timestamp/time.Millisecond),
				)
			} else {
				s.server.CloseSession(s.id)
				s.server.PublishPubSub(
					events.NewRecordingStopped(s.id, "closed", timestamp/time.Millisecond),
				)
			}
		})

		if err := s.livekit.Init(); err != nil {
			return "", err
		}
	}

	return "", nil
}

func (s *Session) StopRecording() time.Duration {
	var duration time.Duration

	if !s.stopped {
		s.stopped = true
		appstats.Sessions.Dec()

		if s.livekit != nil {
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
			duration = s.webrtc.Close()
		}
	}

	return duration
}
