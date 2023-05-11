package prometheus

import (
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	"net/http"
)

var (
	Requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "requests",
		Help:      "Number of requests to the server",
	},
		[]string{
			"method",
			"status",
		})

	Responses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "responses",
		Help:      "Number of responses from the server",
	},
		[]string{
			"method",
			"status",
		})

	Sessions = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: "recorder",
		Name:      "sessions",
		Help:      "Current number of recorder sessions",
	})
)

func Init() {
	prometheus.MustRegister(Requests)
	prometheus.MustRegister(Responses)
	prometheus.MustRegister(Sessions)
}

func ServePromMetrics(cfg config.Prometheus) {
	if !cfg.Enable {
		return
	}

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		http.ListenAndServe(cfg.ListenAddress, nil)
	}()

	log.Infof("Prometheus metrics exported on %s", cfg.ListenAddress)
}
