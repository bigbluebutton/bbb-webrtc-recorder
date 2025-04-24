package appstats

import (
	"net/http"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
)

type metricsHandler struct {
	next      http.Handler
	statsChan chan *CaptureStats
}

var (
	Requests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "in_requests",
		Help:      "Number received by the recorder",
	},
		[]string{
			"method",
		})

	InvalidRequests = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "invalid_requests",
		Help:      "Number of invalid requests",
	})

	Responses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "out_responses",
		Help:      "Number of responses from the recorder",
	},
		[]string{
			"method",
		})

	Sessions = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: "recorder",
		Name:      "sessions",
		Help:      "Current number of recorder sessions",
	})

	// TODO implement ActiveTracks tracking (session storage)
	ActiveTracks = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: "recorder",
		Name:      "active_tracks",
		Help:      "Number of active tracks by type",
	},
		[]string{
			"kind",   // audio/video
			"mime",   // mime type (e.g. video/vp8, audio/opus)
			"source", // track source (e.g. camera, microphone)
		})

	PLIRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "pli_requests_total",
		Help:      "Total number of PLI (Picture Loss Indication) requests sent",
	},
		[]string{
			"source", // track source (e.g. camera, screen)
			"mime",   // mime type
		})

	PacketLossBuffer = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: "recorder",
		Name:      "packet_loss_buffer_percentage",
		Help:      "Packet loss percentage (buffer)",
		Buckets:   []float64{0, 0.1, 0.3, 0.5, 0.7, 1, 5, 10, 40, 100},
	},
		[]string{
			"source", // track source (e.g. camera, screen)
			"kind",   // audio/video
			"mime",   // mime type
		})

	// ----- Recorder-level metrics -----

	SampleDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: "recorder",
		Name:      "sample_duration_ms",
		Help:      "Sample duration in milliseconds",
		Buckets:   []float64{10, 20, 40, 80, 160, 320, 640, 1280, 2560, 5120, 10240},
	},
		[]string{
			"source", // track source (e.g. camera, screen)
			"kind",   // audio/video
			"mime",   // mime type
		})

	FrameSize = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: "recorder",
		Name:      "frame_size_bytes",
		Help:      "Frame size in bytes",
		Buckets:   prometheus.ExponentialBuckets(1024, 2, 10), // 1KB to 1MB
	},
		[]string{
			"source", // track source (e.g. camera, screen)
			"kind",   // audio/video
			"mime",   // mime type
		})

	RTPDiscontinuities = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "rtp_discontinuities_total",
		Help:      "Total number of RTP sequence number discontinuities",
	},
		[]string{
			"source", // track source (e.g. camera, screen)
			"kind",   // audio/video
			"mime",   // mime type
		})

	VP8PictureIDDiscontinuities = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "vp8_picture_id_discontinuities_total",
		Help:      "Total number of VP8 Picture ID discontinuities",
	},
		[]string{
			"source", // track source (e.g. camera, screen)
			"mime",   // mime type
		})

	KeyframeCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "keyframes_total",
		Help:      "Total number of keyframes",
	},
		[]string{
			"source", // track source (e.g. camera, screen)
			"mime",   // mime type
		})

	CorruptedFrames = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "corrupted_frames_total",
		Help:      "Total number of corrupted frames",
	},
		[]string{
			"source", // track source (e.g. camera, screen)
			"mime",   // mime type
		})

	WrittenSamples = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "written_samples_total",
		Help:      "Total number of samples written to the output file",
	},
		[]string{
			"source", // track source (e.g. camera, screen)
			"kind",   // audio/video
			"mime",   // mime type
		})

	RTPReadErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "rtp_read_errors_total",
		Help:      "Total number of RTP read errors",
	},
		[]string{
			"source", // track source (e.g. camera, screen)
			"kind",   // audio/video
			"mime",   // mime type
			"error",  // error string
		})
)

func Init() {
	prometheus.MustRegister(Requests)
	prometheus.MustRegister(InvalidRequests)
	prometheus.MustRegister(Responses)
	prometheus.MustRegister(Sessions)
	prometheus.MustRegister(ActiveTracks)
	prometheus.MustRegister(PLIRequests)
	prometheus.MustRegister(PacketLossBuffer)
	prometheus.MustRegister(SampleDuration)
	prometheus.MustRegister(FrameSize)
	prometheus.MustRegister(RTPDiscontinuities)
	prometheus.MustRegister(VP8PictureIDDiscontinuities)
	prometheus.MustRegister(KeyframeCount)
	prometheus.MustRegister(CorruptedFrames)
	prometheus.MustRegister(WrittenSamples)
	prometheus.MustRegister(RTPReadErrors)
}

func newMetricsHandler() *metricsHandler {
	return &metricsHandler{
		next:      promhttp.Handler(),
		statsChan: make(chan *CaptureStats, 1),
	}
}

func (h *metricsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case stats := <-h.statsChan:
		UpdateCaptureMetrics(stats)
	default:
	}
	h.next.ServeHTTP(w, r)
}

// UpdateStats sends new stats to be processed during the next metrics scrape
func (h *metricsHandler) UpdateStats(stats *CaptureStats) {
	select {
	case h.statsChan <- stats:
	default:
		log.Warn("Stats update dropped - metrics channel full")
	}
}

var (
	// Global metrics handler instance
	metricsHandlerInstance *metricsHandler
)

func ServePromMetrics(cfg config.Prometheus) {
	if !cfg.Enable {
		return
	}

	metricsHandlerInstance = newMetricsHandler()
	http.Handle("/metrics", metricsHandlerInstance)

	go func() {
		if err := http.ListenAndServe(cfg.ListenAddress, nil); err != nil {
			log.Errorf("failed to start metrics server: %s", err)
		}
	}()

	log.Infof("Prometheus metrics exported on %s", cfg.ListenAddress)
}

func TrackRecordingStarted(kind string, mime string, source string) {
	ActiveTracks.With(prometheus.Labels{
		"kind":   kind,
		"mime":   mime,
		"source": source,
	}).Inc()
}

func TrackRecordingStopped(kind string, mime string, source string) {
	ActiveTracks.With(prometheus.Labels{
		"kind":   kind,
		"mime":   mime,
		"source": source,
	}).Dec()
}

func OnRTPReadError(source string, kind string, mime string, error string) {
	RTPReadErrors.With(prometheus.Labels{
		"source": source,
		"kind":   kind,
		"mime":   mime,
		"error":  error,
	}).Inc()
}

func OnServerRequest(event *events.Event) {
	if event.IsValid() {
		Requests.WithLabelValues(event.Id).Inc()
	} else {
		InvalidRequests.Inc()
	}
}

func OnServerResponse(msg interface{}) {
	switch v := msg.(type) {
	case *events.Event:
		Responses.WithLabelValues(v.Id).Inc()
	case *events.StartRecordingResponse:
		Responses.WithLabelValues(events.StartRecordingResponseKey).Inc()
	case *events.RecordingRtpStatusChanged:
		Responses.WithLabelValues(events.RecordingRtpStatusChangedKey).Inc()
	case *events.RecordingStopped:
		Responses.WithLabelValues(events.RecordingStoppedKey).Inc()
	case *events.RecorderStatus:
		Responses.WithLabelValues(events.RecorderStatusKey).Inc()
	default:
		Responses.WithLabelValues("unknown").Inc()
	}
}

func UpdateCaptureMetrics(stats *CaptureStats) {
	if stats == nil {
		return
	}

	for _, trackStats := range stats.Tracks {
		if trackStats == nil {
			continue
		}

		if trackStats.Adapter != nil {
			PLIRequests.With(prometheus.Labels{
				"source": trackStats.Source,
				"mime":   trackStats.MimeType,
			}).Add(float64(trackStats.Adapter.PLIRequests))
		}

		if trackStats.Buffer != nil {
			if trackStats.Buffer.PacketsPushed > 0 {
				packetLoss := float64(trackStats.Buffer.PacketsDropped) / float64(trackStats.Buffer.PacketsPushed) * 100
				PacketLossBuffer.With(prometheus.Labels{
					"source": trackStats.Source,
					"kind":   trackStats.TrackKind,
					"mime":   trackStats.MimeType,
				}).Observe(packetLoss)
			}

			if trackStats.RecorderTrackStats != nil {
				SampleDuration.With(prometheus.Labels{
					"source": trackStats.Source,
					"kind":   trackStats.TrackKind,
					"mime":   trackStats.MimeType,
				}).Observe(float64(trackStats.RecorderTrackStats.AvgSampleDurationMs))

				FrameSize.With(prometheus.Labels{
					"source": trackStats.Source,
					"kind":   trackStats.TrackKind,
					"mime":   trackStats.MimeType,
				}).Observe(float64(trackStats.RecorderTrackStats.AvgFrameSizeBytes))

				RTPDiscontinuities.With(prometheus.Labels{
					"source": trackStats.Source,
					"kind":   trackStats.TrackKind,
					"mime":   trackStats.MimeType,
				}).Add(float64(trackStats.RecorderTrackStats.RTPDiscontInfo.Count))

				VP8PictureIDDiscontinuities.With(prometheus.Labels{
					"source": trackStats.Source,
					"mime":   trackStats.MimeType,
				}).Add(float64(trackStats.RecorderTrackStats.VP8PicIDDiscontInfo.Count))

				KeyframeCount.With(prometheus.Labels{
					"source": trackStats.Source,
					"mime":   trackStats.MimeType,
				}).Add(float64(trackStats.RecorderTrackStats.KeyframeCount))

				CorruptedFrames.With(prometheus.Labels{
					"source": trackStats.Source,
					"mime":   trackStats.MimeType,
				}).Add(float64(trackStats.RecorderTrackStats.CorruptedFrames))

				WrittenSamples.With(prometheus.Labels{
					"source": trackStats.Source,
					"kind":   "video",
					"mime":   trackStats.MimeType,
				}).Add(float64(trackStats.RecorderTrackStats.WrittenSamples))
			}

			if trackStats.RecorderTrackStats != nil {
				SampleDuration.With(prometheus.Labels{
					"source": trackStats.Source,
					"kind":   "audio",
					"mime":   trackStats.MimeType,
				}).Observe(float64(trackStats.RecorderTrackStats.AvgSampleDurationMs))

				RTPDiscontinuities.With(prometheus.Labels{
					"source": trackStats.Source,
					"kind":   "audio",
					"mime":   trackStats.MimeType,
				}).Add(float64(trackStats.RecorderTrackStats.RTPDiscontInfo.Count))

				WrittenSamples.With(prometheus.Labels{
					"source": trackStats.Source,
					"kind":   "audio",
					"mime":   trackStats.MimeType,
				}).Add(float64(trackStats.RecorderTrackStats.WrittenSamples))
			}
		}
	}
}
