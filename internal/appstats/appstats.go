package appstats

import (
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
)

var (
	AppStartTime time.Time
)

func Init() {
	AppStartTime = time.Now()
}

type AdapterTrackStats struct {
	StartTime         int64  `json:"startTime"`
	EndTime           int64  `json:"endTime"`
	FirstSeqNum       uint16 `json:"firstSeqNum"`
	LastSeqNum        uint16 `json:"lastSeqNum"`
	SeqNumWrapArounds int    `json:"seqNumWrapArounds"`
	PLIRequests       int    `json:"pliRequests"`
	RTPReadErrors     int    `json:"rtpReadErrors"`
}

type BufferStatsWrapper struct {
	PacketsPushed  uint64 `json:"packetsPushed"`
	PacketsPopped  uint64 `json:"packetsPopped"`
	PacketsDropped uint64 `json:"packetsDropped"`
	PaddingPushed  uint64 `json:"paddingPushed"`
	SamplesPopped  uint64 `json:"samplesPopped"`
}

type TrackStats struct {
	Source             string                    `json:"source"`
	Buffer             *BufferStatsWrapper       `json:"buffer"`
	Adapter            *AdapterTrackStats        `json:"adapter"`
	RecorderTrackStats *types.RecorderTrackStats `json:"recorder"`
	TrackKind          string                    `json:"trackKind"`
	MimeType           string                    `json:"mimeType"`
}

const (
	ConstraintEndReleased       = "released"
	ConstraintEndCaptureStopped = "capture_stopped"
)

// ConstraintRecord is one interval during which a constraint lowered the
// quality of a capture. Times are Unix milliseconds; an open interval has
// a zero EndTimeMs and an empty EndReason.
type ConstraintRecord struct {
	Reason       string   `json:"reason"`
	VideoQuality string   `json:"videoQuality"`
	TrackIDs     []string `json:"trackIds"`
	StartTimeMs  int64    `json:"startTimeMs"`
	EndTimeMs    int64    `json:"endTimeMs"`
	EndReason    string   `json:"endReason"`
}

type CaptureStats struct {
	RecorderSessionUUID string                 `json:"recorderSessionUuid"`
	RoomID              string                 `json:"roomId"`
	ParticipantID       string                 `json:"participantId"`
	Tracks              map[string]*TrackStats `json:"tracks"`
	FileName            string                 `json:"fileName"`
	Constrained         bool                   `json:"constrained"`
	Constraints         []ConstraintRecord     `json:"constraints"`
}

func GetUptime() time.Duration {
	return time.Since(AppStartTime)
}

func ObserveRequestDuration(method string, duration time.Duration) {
	RequestDuration.WithLabelValues(method).Observe(duration.Seconds())
}

func OnSessionError(reason string) {
	SessionErrors.WithLabelValues(reason).Inc()
}

func ObserveLiveKitConnectDuration(duration time.Duration) {
	LiveKitConnectDuration.Observe(duration.Seconds())
}

func ObserveLiveKitSubscribeDuration(duration time.Duration) {
	LiveKitSubscribeDuration.Observe(duration.Seconds())
}

func OnCaptureConstrained(reason string, firstForReason bool) {
	CaptureConstraintChanges.WithLabelValues(reason, "apply").Inc()
	ConstrainedCapturesActive.WithLabelValues(reason).Inc()

	// firstForReason is true the first time the capture is constrained for that reason.
	if firstForReason {
		ConstrainedCapturesTotal.WithLabelValues(reason).Inc()
	}
}

func OnCaptureConstraintLifted(reason string) {
	CaptureConstraintChanges.WithLabelValues(reason, "lift").Inc()
	ConstrainedCapturesActive.WithLabelValues(reason).Dec()
}

// OnConstrainedCaptureStopped records a capture that stopped while still
// constrained. Same metric as lifted, different trigger/nature.
func OnConstrainedCaptureStopped(reason string) {
	ConstrainedCapturesActive.WithLabelValues(reason).Dec()
}
