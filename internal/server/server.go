package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	log "github.com/sirupsen/logrus"
	"sync"
	"time"
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
	log.Debug(string(msg))
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

		flowCallbackFn := func() recorder.FlowCallbackFn {
			return func(isFlowing bool, keyframeSequence int64, videoTimestamp time.Duration, closed bool) {
				var message interface{}
				if !closed {
					message = events.NewRecordingRtpStatusChanged(e.SessionId, isFlowing, videoTimestamp/time.Millisecond)
				} else {
					s.CloseSession(e.SessionId)
					message = events.NewRecordingStopped(e.SessionId, "closed", videoTimestamp/time.Millisecond)
				}
				s.PublishPubSub(message)
			}
		}

		var sdp string
		if rec, err := recorder.NewRecorder(ctx, s.cfg.Recorder, e.FileName, flowCallbackFn()); err != nil {
			log.WithField("session", ctx.Value("session")).
				Error(err)
			s.PublishPubSub(e.Fail(err))
		} else {
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("%v", r)
					}
				}()

				wrtc := webrtc.NewWebRTC(ctx, s.cfg.WebRTC)
				sess := NewSession(e.SessionId, s, wrtc, rec)
				s.sessions.Store(e.SessionId, sess)
				sdp = sess.StartRecording(e.SDP)
				s.PublishPubSub(e.Success(sdp, rec.GetFilePath()))
			}()
			if err != nil {
				log.WithField("session", ctx.Value("session")).
					Error(err)
				s.PublishPubSub(e.Fail(err))
			}
		}
	case "stopRecording":
		e := event.StopRecording()
		if sess, ok := s.sessions.Load(e.SessionId); !ok {
			s.PublishPubSub(e.Stopped("session not found", 0))
		} else {
			ts := sess.(*Session).StopRecording() / time.Millisecond
			s.sessions.Delete(e.SessionId)
			s.PublishPubSub(e.Stopped("stop requested", ts))
		}
	}
}

func (s *Server) PublishPubSub(msg interface{}) {
	j, _ := json.Marshal(msg)
	s.pubsub.Publish(s.cfg.PubSub.Channels.Publish, j)
}

func (s *Server) CloseSession(id string) {
	s.sessions.Delete(id)
}
