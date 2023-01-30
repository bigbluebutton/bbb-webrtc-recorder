package signal

import (
	"fmt"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub"
	"github.com/bigbluebutton/bbb-webrtc-recorder/web"
	log "github.com/sirupsen/logrus"
	"io"
	"io/fs"
	"net/http"
	"strconv"
)

type HTTPServer struct {
	port      int
	mediaRoot string
	publicFS  fs.FS
	pubsub    pubsub.PubSub
}

func NewHTTPServer(port int, mediaRoot string, ps pubsub.PubSub) *HTTPServer {
	return &HTTPServer{
		port:      port,
		mediaRoot: mediaRoot,
		publicFS:  web.PublicEmbedFS,
		pubsub:    ps,
	}
}

func (s *HTTPServer) Serve(handler func(string) string) {

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
	log.Println("Starting server on " + addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}
