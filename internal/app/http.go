package app

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/signal"
	"path"
	"time"
)

func http() {
	if !cfg.HTTP.Enable {
		return
	}
	ps := pubsub.NewPubSub(cfg.PubSub)
	h := signal.NewHTTPServer(8080, path.Clean(cfg.Recorder.Directory), ps)
	answerChannels := make(map[string]*chan string)
	go ps.Subscribe(cfg.PubSub.Channel, func(ctx context.Context, msg []byte) {
		name, event := events.Decode(msg)
		if name == "startRecordingResponse" {
			e := event.(events.StartRecordingResponse)
			*answerChannels[e.RecordingSessionId] <- *e.SDP
		}
	})
	h.Serve(func(sdp string) string {
		t := time.Now()
		sessId := t.Format("20060102150405")
		answerChan := make(chan string)
		answerChannels[sessId] = &answerChan
		e := events.StartRecording{
			Id:                 "startRecording",
			RecordingSessionId: sessId,
			SDP:                sdp,
			FileName:           fmt.Sprintf("%s.webm", t.Format("20060102150405")),
		}
		j, _ := json.Marshal(e)
		ps.Publish(cfg.PubSub.Channel, j)
		answer := <-answerChan
		delete(answerChannels, sessId)
		return answer
	})
}
