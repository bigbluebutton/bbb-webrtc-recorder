package appstats

import (
	"math"
	"net/http"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/pubsub/events"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
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

	ParticipantReconnectingEvents = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "participant_reconnecting_events_total",
		Help:      "Total number of participant reconnecting events",
	})

	ParticipantReconnects = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "participant_reconnects_total",
		Help:      "Total number of participant reconnects",
	})

	TrackSubscriptionFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "track_subscription_failures_total",
		Help:      "Total number of track subscription failures",
	},
		[]string{
			"error", // error string
		})

	ComponentHealth = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: "recorder",
		Name:      "component_health",
		Help:      "Health status of recorder components (1 = healthy, 0 = unhealthy)",
	},
		[]string{
			"component",
		})

	RequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: "recorder",
		Name:      "request_duration_seconds",
		Help:      "Duration of server requests.",
		Buckets:   []float64{0.01, 0.02, 0.03, 0.05, 0.1, 0.3, 0.5, 1.0, 2.0, 5.0},
	},
		[]string{
			"method",
		})

	SessionErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "session_errors_total",
		Help:      "Total number of session-related errors.",
	},
		[]string{
			"reason",
		})

	LiveKitConnectDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Subsystem: "recorder",
		Name:      "livekit_connect_duration_seconds",
		Help:      "Duration of the LiveKit room connect procedure.",
		Buckets:   []float64{0.01, 0.02, 0.03, 0.05, 0.1, 0.3, 0.5, 1.0, 2.0, 5.0},
	})

	LiveKitSubscribeDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Subsystem: "recorder",
		Name:      "livekit_subscribe_duration_seconds",
		Help:      "Duration of the LiveKit track subscribe procedure.",
		Buckets:   []float64{0.01, 0.02, 0.03, 0.05, 0.1, 0.3, 0.5, 1.0, 2.0, 5.0},
	})

	ConstrainedCapturesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "constrained_captures_total",
		Help:      "Total number of captures constrained at least once, counted once per capture and reason.",
	},
		[]string{
			"reason",
		})

	ConstrainedCapturesActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Subsystem: "recorder",
		Name:      "constrained_captures_active",
		Help:      "Current number of constrained captures.",
	},
		[]string{
			"reason",
		})

	CaptureConstraintChanges = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "capture_constraint_changes_total",
		Help:      "Total number of constraints applied to or lifted from captures.",
	},
		[]string{
			"reason",
			"action", // apply/lift
		})

	DiskPressureEnabled = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: "recorder",
		Name:      "disk_pressure_enabled",
		Help:      "Whether the disk pressure guard is running (1) or not (0).",
	})

	DiskPressureActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: "recorder",
		Name:      "disk_pressure_active",
		Help:      "Whether the disk pressure guard is engaged (1) or not (0).",
	})

	DiskFreeBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: "recorder",
		Name:      "disk_free_bytes",
		Help:      "Bytes available to unprivileged users on the recorder's configured filesystem.",
	})

	DiskTotalBytes = prometheus.NewGauge(prometheus.GaugeOpts{
		Subsystem: "recorder",
		Name:      "disk_total_bytes",
		Help:      "Size in bytes of the recorder's configured filesystem.",
	})

	DiskPressureTransitions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "disk_pressure_transitions_total",
		Help:      "Total number of disk pressure guard state changes.",
	},
		[]string{
			"state", // engaged/released
		})

	DiskCheckErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "disk_check_errors_total",
		Help:      "Total number of failed disk pressure guard checks.",
	})

	// ----- Media clock (microphones) -----

	MediaClockRateRatio = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: "recorder",
		Name:      "media_clock_rate_ratio",
		Help: "Media time a track's RTP timestamps covered over the time its packets took to arrive, " +
			"less LiveKit's realignments, per track of 10 s or more: below 1 when the sender's media clock runs slow.",
		Buckets: []float64{0.9, 0.95, 0.97, 0.98, 0.99, 0.995, 0.999, 1.001, 1.005, 1.01, 1.03},
	},
		[]string{"source", "kind", "mime"})

	MediaClockInsertedGaps = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "media_clock_inserted_gaps_total",
		Help:      "Total number of timestamp gaps inserted because a sender's media clock fell behind the wall clock.",
	},
		[]string{"source", "kind", "mime"})

	MediaClockInsertedGapSeconds = prometheus.NewCounterVec(prometheus.CounterOpts{
		Subsystem: "recorder",
		Name:      "media_clock_inserted_gap_seconds_total",
		Help:      "Total media time inserted as timestamp gaps, in seconds.",
	},
		[]string{"source", "kind", "mime"})

	MediaClockEndLag = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Subsystem: "recorder",
		Name:      "media_clock_end_lag_seconds",
		Help: "How far behind the wall clock a track's packets arrived at its end, after its gaps, per track of 10 s or more: " +
			"how early the recording plays its end, unless a network delay grew near the end. A track ahead counts as 0.",
		Buckets: []float64{0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 30, 60},
	},
		[]string{"source", "kind", "mime"})
)

// The rate and lag of tracks shorter than this are mostly start-up noise.
const mediaClockMinDuration = 10 * time.Second

func RegisterMetrics() {
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
	prometheus.MustRegister(ParticipantReconnects)
	prometheus.MustRegister(TrackSubscriptionFailures)
	prometheus.MustRegister(ParticipantReconnectingEvents)
	prometheus.MustRegister(ComponentHealth)
	prometheus.MustRegister(RequestDuration)
	prometheus.MustRegister(SessionErrors)
	prometheus.MustRegister(LiveKitConnectDuration)
	prometheus.MustRegister(LiveKitSubscribeDuration)
	prometheus.MustRegister(ConstrainedCapturesTotal)
	prometheus.MustRegister(ConstrainedCapturesActive)
	prometheus.MustRegister(CaptureConstraintChanges)
	prometheus.MustRegister(DiskPressureEnabled)
	prometheus.MustRegister(DiskPressureActive)
	prometheus.MustRegister(DiskFreeBytes)
	prometheus.MustRegister(DiskTotalBytes)
	prometheus.MustRegister(DiskPressureTransitions)
	prometheus.MustRegister(DiskCheckErrors)
	prometheus.MustRegister(MediaClockRateRatio)
	prometheus.MustRegister(MediaClockInsertedGaps)
	prometheus.MustRegister(MediaClockInsertedGapSeconds)
	prometheus.MustRegister(MediaClockEndLag)

	reason := types.ConstraintReasonDiskPressure
	ConstrainedCapturesTotal.WithLabelValues(reason)
	ConstrainedCapturesActive.WithLabelValues(reason)
	CaptureConstraintChanges.WithLabelValues(reason, "apply")
	CaptureConstraintChanges.WithLabelValues(reason, "lift")
	DiskPressureTransitions.WithLabelValues("engaged")
	DiskPressureTransitions.WithLabelValues("released")

	DiskFreeBytes.Set(math.NaN())
	DiskTotalBytes.Set(math.NaN())
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

func OnTrackRecordingStarted(kind string, mime string, source string) {
	ActiveTracks.With(prometheus.Labels{
		"kind":   kind,
		"mime":   mime,
		"source": source,
	}).Inc()
}

func OnTrackRecordingStopped(kind string, mime string, source string) {
	ActiveTracks.With(prometheus.Labels{
		"kind":   kind,
		"mime":   mime,
		"source": source,
	}).Dec()
}

func OnParticipantReconnecting() {
	ParticipantReconnectingEvents.Inc()
}

func OnParticipantReconnected() {
	ParticipantReconnects.Inc()
}

func OnTrackSubscriptionFailed(error string) {
	TrackSubscriptionFailures.WithLabelValues(error).Inc()
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

func SetComponentHealth(component string, healthy bool) {
	status := 0.0

	if healthy {
		status = 1.0
	}

	ComponentHealth.WithLabelValues(component).Set(status)
}

func UpdateCaptureMetrics(stats *CaptureStats) {
	if stats == nil {
		return
	}

	for _, trackStats := range stats.Tracks {
		if trackStats == nil {
			continue
		}

		if trackStats.MediaClock != nil {
			updateMediaClockMetrics(trackStats)
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

func updateMediaClockMetrics(trackStats *TrackStats) {
	labels := prometheus.Labels{"source": trackStats.Source, "kind": trackStats.TrackKind, "mime": trackStats.MimeType}
	mc := trackStats.MediaClock

	MediaClockInsertedGaps.With(labels).Add(float64(mc.InsertedGaps))
	MediaClockInsertedGapSeconds.With(labels).Add(float64(mc.InsertedGapMs) / 1000)

	if time.Duration(mc.ArrivalDurationMs)*time.Millisecond >= mediaClockMinDuration {
		MediaClockRateRatio.With(labels).Observe(mc.RateRatio)
		MediaClockEndLag.With(labels).Observe(float64(max(mc.LagMs, 0)) / 1000)
	}
}
