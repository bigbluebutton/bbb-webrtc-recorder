package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

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
}

func NewServer(cfg *config.Config, ps pubsub.PubSub) *Server {
	return &Server{cfg: cfg, pubsub: ps}
}

func (s *Server) HandlePubSub(ctx context.Context, msg []byte) {
	log.Trace(string(msg))
	event := events.Decode(msg)

	if !event.IsValid() {
		return
	}

	switch event.Id {
	case "startRecording":
		e := event.StartRecording()

		if e == nil {
			s.PublishPubSub(e.Fail(fmt.Errorf("incorrect event")))
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

		switch e.Adapter {
		case "livekit":
			rec, err = recorder.NewRecorder(ctx, s.cfg.Recorder, e.FileName)

			if err != nil {
				log.WithField("session", ctx.Value("session")).Error(err)
				s.PublishPubSub(e.Fail(err))
				return
			}

			wrtc := livekit.NewLiveKitWebRTC(
				ctx,
				s.cfg.LiveKit,
				rec,
				e.AdapterOptions.LiveKit.Room,
				e.AdapterOptions.LiveKit.TrackIDs,
			)
			sess := NewSession(e.SessionId, s, nil, wrtc, rec)
			s.sessions.Store(e.SessionId, sess)

			if _, err := sess.StartRecording(e); err != nil {
				log.WithField("session", ctx.Value("session")).Error(err)
				s.PublishPubSub(e.Fail(err))
				return
			}

			s.PublishPubSub(e.Success("", rec.GetFilePath()))

		case "mediasoup", "":
			rec, err = recorder.NewRecorder(ctx, s.cfg.Recorder, e.FileName)

			if err != nil {
				log.WithField("session", ctx.Value("session")).Error(err)
				s.PublishPubSub(e.Fail(err))
				return
			}

			var sdp string
			func() {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("%v", r)
					}
				}()

				wrtc := webrtc.NewWebRTC(ctx, s.cfg.WebRTC, rec)
				sess := NewSession(e.SessionId, s, wrtc, nil, rec)
				s.sessions.Store(e.SessionId, sess)
				sdp, err = sess.StartRecording(e)
				s.PublishPubSub(e.Success(sdp, rec.GetFilePath()))
			}()

			if err != nil {
				log.WithField("session", ctx.Value("session")).Error(err)
				s.PublishPubSub(e.Fail(err))
			}

		default:
			err := fmt.Errorf("unknown adapter type: %s", e.Adapter)
			log.WithField("session", ctx.Value("session")).Error(err)
			s.PublishPubSub(e.Fail(err))
			return
		}

	case "stopRecording":
		e := event.StopRecording()

		if e == nil {
			return
		}

		if sess, ok := s.sessions.Load(e.SessionId); ok {
			ts := sess.(*Session).StopRecording() / time.Millisecond
			s.PublishPubSub(e.Stopped("stopped", ts))
			s.CloseSession(e.SessionId)
		}

	case "getRecorderStatus":
		s.PublishPubSub(events.NewRecorderStatus(s.cfg.App.Version, s.cfg.App.InstanceId))
	}
}

func (s *Server) PublishPubSub(msg interface{}) {
	j, _ := json.Marshal(msg)
	s.pubsub.Publish(s.cfg.PubSub.Channels.Publish, j)
}

func (s *Server) OnStart() error {
	log.Info("Application started. Version=", s.cfg.App.Version, " InstanceId=", s.cfg.App.InstanceId)
	s.PublishPubSub(events.NewRecorderStatus(s.cfg.App.Version, s.cfg.App.InstanceId))
	return nil
}

func (s *Server) CloseSession(id string) {
	s.sessions.Delete(id)
}
