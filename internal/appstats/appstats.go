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

type CaptureStats struct {
	RecorderSessionUUID string                 `json:"recorderSessionUuid"`
	RoomID              string                 `json:"roomId"`
	ParticipantID       string                 `json:"participantId"`
	Tracks              map[string]*TrackStats `json:"tracks"`
	FileName            string                 `json:"fileName"`
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
