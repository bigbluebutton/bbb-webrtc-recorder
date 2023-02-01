package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/AlekSi/pointer"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/server/session"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	log "github.com/sirupsen/logrus"
	"time"
)

type Server struct {
	cfg    *config.Config
	pubsub pubsub.PubSub
}

var sessions = make(map[string]*session.Session)

func NewServer(cfg *config.Config, pubsub pubsub.PubSub) *Server {
	return &Server{cfg: cfg, pubsub: pubsub}
}

func (s *Server) HandlePubSub(ctx context.Context, msg []byte) {
	name, event := events.Decode(msg)
	log.Debug(string(msg))

	switch name {
	case "startRecording":
		e := event.(events.StartRecording)
		response := events.StartRecordingResponse{
			Id:                 "startRecordingResponse",
			RecordingSessionId: e.RecordingSessionId,
		}

		ctx = context.WithValue(ctx, "session", e.RecordingSessionId)

		if _, ok := sessions[e.RecordingSessionId]; ok {
			err := fmt.Errorf("session %s already exists", e.RecordingSessionId)
			log.Error(err)
			response.Status = "failed"
			response.Error = pointer.ToString(err.Error())
			s.PublishPubSub(response)
		}

		flowCallbackFn := func() recorder.FlowCallbackFn {
			return func(isFlowing bool, keyframeSequence int64, videoTimestamp time.Duration) {
				message := events.RecordingRtpStatusChanged{
					Id:                 "recordingRtpStatusChanged",
					RecordingSessionId: e.RecordingSessionId,
					Status:             events.FlowingStatus[isFlowing],
					TimestampUTC:       time.Now().UTC(),
					TimestampHR:        videoTimestamp / time.Millisecond,
				}
				s.PublishPubSub(message)
			}
		}

		var sdp string
		if rec, err := recorder.NewRecorder(ctx, s.cfg.Recorder, e.FileName, flowCallbackFn()); err != nil {
			log.Error(err)
			response.Status = "failed"
			response.Error = pointer.ToString(err.Error())
		} else {
			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("%v", r)
					}
				}()

				wrtc := webrtc.NewWebRTC(ctx, s.cfg.WebRTC)
				sess := session.NewSession(wrtc, rec)
				sessions[e.RecordingSessionId] = sess
				sdp = sess.StartRecording(e)
				response.Status = "ok"
				response.SDP = &sdp
				log.WithField("session", ctx.Value("session")).
					Debug(sdp)
			}()
			if err != nil {
				log.WithField("session", ctx.Value("session")).
					Error(err)
				response.Status = "failed"
				response.Error = pointer.ToString(err.Error())
			}
		}
		s.PublishPubSub(response)
	case "stopRecording":
		e := event.(events.StopRecording)
		response := events.RecordingStopped{
			Id:                 "recordingStopped",
			RecordingSessionId: e.RecordingSessionId,
		}
		if sess, ok := sessions[e.RecordingSessionId]; !ok {
			response.Reason = "session not found"
		} else {
			sess.StopRecording()
			delete(sessions, e.RecordingSessionId)
			response.Reason = "stop requested"
		}
		s.PublishPubSub(response)
	}
}

func (s *Server) PublishPubSub(msg interface{}) {
	j, _ := json.Marshal(msg)
	s.pubsub.Publish(s.cfg.PubSub.Channels.Publish, j)
}
