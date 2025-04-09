package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/bigbluebutton/bbb-webrtc-recorder/web"
	log "github.com/sirupsen/logrus"
)

type HTTPServer struct {
	cfg       *config.Config
	port      int
	mediaRoot string
	publicFS  fs.FS
	pubsub    pubsub.PubSub
}

func NewHTTPServer(cfg *config.Config, ps pubsub.PubSub) *HTTPServer {
	return &HTTPServer{
		cfg:       cfg,
		port:      cfg.HTTP.Port,
		mediaRoot: path.Clean(cfg.Recorder.Directory),
		publicFS:  web.PublicEmbedFS,
		pubsub:    ps,
	}
}

func (s *HTTPServer) Serve() {
	answerChannels := make(map[string]*chan string)
	go s.pubsub.Subscribe(s.cfg.PubSub.Channels.Publish,
		func(ctx context.Context, msg []byte) {
			event := events.Decode(msg)
			if e := event.StartRecordingResponse(); e != nil {
				if e.SDP != nil {
					*answerChannels[e.SessionId] <- *e.SDP
				}
			}
		},
		func() error {
			return nil
		},
	)
	go s.serve(func(sdp string) string {
		t := time.Now()
		sessId := t.Format("20060102150405")
		answerChan := make(chan string)
		answerChannels[sessId] = &answerChan
		e := events.StartRecording{
			Id:        "startRecording",
			SessionId: sessId,
			FileName:  fmt.Sprintf("%s.webm", t.Format("20060102150405")),
			Adapter:   "mediasoup",
			AdapterOptions: &events.AdapterOptions{
				Mediasoup: &events.MediasoupConfig{
					SDP: sdp,
				},
			},
		}
		j, _ := json.Marshal(e)
		s.pubsub.Publish(s.cfg.PubSub.Channels.Subscribe, j)
		answer := <-answerChan
		delete(answerChannels, sessId)
		return answer
	})
}

func (s *HTTPServer) serve(handler func(string) string) {

	http.HandleFunc("/sdp", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if len(body) == 0 || r.Method != http.MethodPost {
			w.WriteHeader(422)
			fmt.Fprintf(w, "Post SDP offer in body")
			return
		}
		response := handler(string(body))
		fmt.Fprintf(w, response)
	})

	http.Handle("/media/", http.StripPrefix("/media", http.FileServer(http.Dir(s.mediaRoot))))

	public, _ := fs.Sub(s.publicFS, "public")
	http.Handle("/", http.FileServer(http.FS(public)))
	http.HandleFunc("/favicon.ico", func(rw http.ResponseWriter, r *http.Request) {})

	addr := ":" + strconv.Itoa(s.port)
	log.Printf("starting http server on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}
