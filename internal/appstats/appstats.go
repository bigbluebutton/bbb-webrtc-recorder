package appstats

import (
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
)

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
	Source             string                       `json:"source"`
	Buffer             *BufferStatsWrapper          `json:"buffer"`
	Adapter            *AdapterTrackStats           `json:"adapter"`
	RecorderTrackStats *recorder.RecorderTrackStats `json:"recorderTrackStats,omitempty"`
	TrackKind          string                       `json:"trackKind"`
	MimeType           string                       `json:"mimeType"`
}

type CaptureStats struct {
	RecorderSessionUUID string                 `json:"recorderSessionId"`
	RoomID              string                 `json:"roomId"`
	ParticipantID       string                 `json:"participantId"`
	Tracks              map[string]*TrackStats `json:"tracks"`
	FileName            string                 `json:"fileName"`
}
