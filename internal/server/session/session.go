package session

import (
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/signal"
	pwebrtc "github.com/pion/webrtc/v3"
)

type Session struct {
	webrtc   *webrtc.WebRTC
	recorder recorder.Recorder
}

func NewSession(wrtc *webrtc.WebRTC, recorder recorder.Recorder) *Session {
	return &Session{
		webrtc:   wrtc,
		recorder: recorder,
	}
}

func (s *Session) StartRecording(e events.StartRecording) string {
	offer := pwebrtc.SessionDescription{}
	signal.Decode(e.SDP, &offer)
	sdp := s.webrtc.Init(offer, s.recorder)
	return signal.Encode(sdp)
}

func (s *Session) StopRecording() {
	//s.webrtc.Close()
	s.recorder.Close()
}
