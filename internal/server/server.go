package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/livekit"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	log "github.com/sirupsen/logrus"
)

type Server struct {
	cfg      *config.Config
	pubsub   pubsub.PubSub
	sessions sync.Map
	// shutdownWg is used to wait for graceful shutdowns
	shutdownWg sync.WaitGroup
}

func NewServer(cfg *config.Config, ps pubsub.PubSub) *Server {
	return &Server{cfg: cfg, pubsub: ps}
}

func (s *Server) HandlePubSubMsg(ctx context.Context, msg []byte) {
	log.Trace(string(msg))
	event := events.Decode(msg)
	s.HandlePubSubEvent(ctx, event)
}

func (s *Server) HandlePubSubEvent(ctx context.Context, event *events.Event) {
	start := time.Now()
	method := "invalid"
	processedHere := true

	defer func() {
		if processedHere {
			appstats.ObserveRequestDuration(method, time.Since(start))
		}
	}()

	appstats.OnServerRequest(event)

	if !event.IsValid() {
		return
	}

	method = event.Id

	switch event.Id {
	case "startRecording":
		e := event.StartRecording()

		if e == nil {
			s.PublishPubSub(e.Fail(fmt.Errorf("incorrect event")))
			return
		}

		ctx = context.WithValue(ctx, "session", e.SessionId)

		_, ok := s.sessions.Load(e.SessionId)
		if ok {
			err := fmt.Errorf("session %s already exists", e.SessionId)
			log.Error(err)
			s.PublishPubSub(e.Fail(err))
			return
		}

		if err := e.Validate(); err != nil {
			log.WithField("session", ctx.Value("session")).Error(err)
			s.PublishPubSub(e.Fail(err))
			return
		}

		var rec recorder.Recorder
		var err error
		var wrtc *webrtc.WebRTC
		var lk *livekit.LiveKitWebRTC

		switch e.Adapter {
		case "livekit":
			rec, err = recorder.NewRecorder(ctx, s.cfg.Recorder, e.FileName)

			if err != nil {
				log.WithField("session", ctx.Value("session")).Error(err)
				s.PublishPubSub(e.Fail(err))
				return
			}

			lk = livekit.NewLiveKitWebRTC(
				ctx,
				s.cfg.LiveKit,
				rec,
				e.AdapterOptions.LiveKit.Room,
				e.AdapterOptions.LiveKit.TrackIDs,
			)

		case "mediasoup", "":
			rec, err = recorder.NewRecorder(ctx, s.cfg.Recorder, e.FileName)

			if err != nil {
				log.WithField("session", ctx.Value("session")).Error(err)
				s.PublishPubSub(e.Fail(err))
				return
			}

			wrtc = webrtc.NewWebRTC(ctx, s.cfg.WebRTC, rec)

		default:
			err := fmt.Errorf("unknown adapter type: %s", e.Adapter)
			log.WithField("session", ctx.Value("session")).Error(err)
			s.PublishPubSub(e.Fail(err))

			return
		}

		sess := NewSession(e.SessionId, s, wrtc, lk, rec)
		s.sessions.Store(e.SessionId, sess)
		s.shutdownWg.Add(1)
		go sess.Run(&s.shutdownWg)

		if err := sess.StartRecording(e, start); err != nil {
			log.WithField("session", e.SessionId).Errorf("failed to send start command: %v", err)
			s.PublishPubSub(e.Fail(err))
			appstats.OnSessionError(err.Error())

			if stopErr := sess.StopRecording(nil, "start_failed", time.Time{}); stopErr != nil {
				log.WithField("session", e.SessionId).Errorf("failed to stop session after start failure: %v", stopErr)
				appstats.OnSessionError(stopErr.Error())
			}

			return
		}
		processedHere = false

	case "stopRecording":
		e := event.StopRecording()

		if e == nil {
			return
		}

		if sess, ok := s.sessions.Load(e.SessionId); ok {
			if err := sess.(*Session).StopRecording(e, events.StopReasonNormal, start); err != nil {
				log.WithField("session", e.SessionId).Errorf("failed to send stop command: %v", err)
				appstats.OnSessionError(err.Error())
				return
			}
			processedHere = false
		}

	case "getRecorderStatus":
		s.PublishPubSub(events.NewRecorderStatus(s.cfg.App.Version, s.cfg.App.InstanceId))

	case "getRecordings":
		e := event.GetRecordings()

		if e == nil {
			return
		}

		go s.handleGetRecordings(e)
		processedHere = false
	}
}

func (s *Server) handleGetRecordings(e *events.GetRecordings) {
	start := time.Now()
	recordings := make([]*events.RecordingInfo, 0)

	defer func() {
		appstats.ObserveRequestDuration("getRecordings", time.Since(start))
	}()

	s.sessions.Range(func(key, value interface{}) bool {
		session := value.(*Session)

		if info := session.GetRecordingInfo(); info != nil {
			recordings = append(recordings, info)
		}

		return true
	})
	response := &events.GetRecordingsResponse{
		Id:         events.GetRecordingsResponseKey,
		RequestId:  e.RequestId,
		Recordings: recordings,
	}
	s.PublishPubSub(response)
}

func (s *Server) PublishPubSub(msg interface{}) {
	j, _ := json.Marshal(msg)
	s.pubsub.Publish(s.cfg.PubSub.Channels.Publish, j)
	appstats.OnServerResponse(msg)
}

func (s *Server) OnStart() error {
	log.Info("Application started. Version=", s.cfg.App.Version, " InstanceId=", s.cfg.App.InstanceId)
	s.PublishPubSub(events.NewRecorderStatus(s.cfg.App.Version, s.cfg.App.InstanceId))
	return nil
}

func (s *Server) CloseSession(id string) {
	s.sessions.Delete(id)
}

func (s *Server) Close() error {
	// Close all sessions gracefully
	s.sessions.Range(func(key, value interface{}) bool {
		sess := value.(*Session)

		if err := sess.StopRecording(nil, events.StopReasonAppShutdown, time.Time{}); err != nil {
			log.WithField("session", sess.id).Errorf("failed to send stop command during shutdown: %v", err)
		}

		return true
	})

	s.shutdownWg.Wait()
	return nil
}
