package events

import (
	"fmt"
	"time"

	"github.com/AlekSi/pointer"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
)

type Event struct {
	Id   string
	Data interface{}
}

type AdapterType string

const (
	AdapterMediasoup AdapterType = "mediasoup"
	AdapterLiveKit   AdapterType = "livekit"
)

const (
	StartRecordingKey            = "startRecording"
	StartRecordingResponseKey    = "startRecordingResponse"
	RecordingRtpStatusChangedKey = "recordingRtpStatusChanged"
	RecordingMediaStartedKey     = "recordingMediaStarted"
	StopRecordingKey             = "stopRecording"
	RecordingStoppedKey          = "recordingStopped"
	RecorderStatusKey            = "recorderStatus"
	GetRecorderStatusKey         = "getRecorderStatus"
	GetRecordingsKey             = "getRecordings"
	GetRecordingsResponseKey     = "getRecordingsResponse"
)

const (
	StopReasonAppShutdown = "application_shutdown"
	StopReasonNormal      = "stopped"
)

// CapabilityMediaTimestamps, in recorderStatus, says LiveKit sessions send
// recordingMediaStarted and the media span in recordingStopped.
const CapabilityMediaTimestamps = "mediaTimestamps"

type AdapterOptions struct {
	Mediasoup *MediasoupConfig `json:"mediasoup,omitempty"`
	LiveKit   *LiveKitConfig   `json:"livekit,omitempty"`
}

type MediasoupConfig struct {
	SDP string `json:"sdp,omitempty"`
}

type LiveKitConfig struct {
	Room     string   `json:"room,omitempty"`
	TrackIDs []string `json:"trackIds,omitempty"`
}

func (e *Event) IsValid() bool {
	return e.Id != ""
}

func (e *Event) StartRecording() *StartRecording {
	if ev, ok := e.Data.(*StartRecording); ok {
		return ev
	}
	return nil
}

func (e *Event) StartRecordingResponse() *StartRecordingResponse {
	if ev, ok := e.Data.(*StartRecordingResponse); ok {
		return ev
	}
	return nil
}

func (e *Event) StopRecording() *StopRecording {
	if ev, ok := e.Data.(*StopRecording); ok {
		return ev
	}
	return nil
}

/*
startRecording (SFU -> Recorder)
```JSON5

	{
		id: 'startRecording',
		recordingSessionId: <String> // requester-defined - error out if collision.
		sdp?: <String>, // offer
		fileName: <String>, // file name INCLUDING format (.webm)
		adapter: <String>, // "mediasoup" or "livekit"
		adapterOptions: <Object>, // adapter-specific configuration
	}

```
*/
type StartRecording struct {
	Id             string          `json:"id,omitempty"`
	SessionId      string          `json:"recordingSessionId,omitempty"`
	FileName       string          `json:"fileName,omitempty"`
	Adapter        AdapterType     `json:"adapter,omitempty"` // "mediasoup" or "livekit"
	AdapterOptions *AdapterOptions `json:"adapterOptions,omitempty"`
	Metadata       map[string]any  `json:"metadata,omitempty"`
	// Legacy field for backward compatibility - check AdapterOptions#Mediasoup#SDP
	// for the new format
	SDP string `json:"sdp,omitempty"`
}

func (e *StartRecording) Validate() error {
	switch e.Adapter {
	case AdapterLiveKit:
		if e.AdapterOptions == nil || e.AdapterOptions.LiveKit == nil {
			return fmt.Errorf("livekit adapter requires livekit configuration")
		}

		if e.AdapterOptions.LiveKit.Room == "" {
			return fmt.Errorf("livekit adapter requires room name")
		}

		if len(e.AdapterOptions.LiveKit.TrackIDs) == 0 {
			return fmt.Errorf("livekit adapter requires at least one track ID")
		}
	case AdapterMediasoup:
		// TODO remove legacy SDP field later on - prlanzarin
		if e.AdapterOptions != nil && e.AdapterOptions.Mediasoup != nil && e.AdapterOptions.Mediasoup.SDP != "" {
			return nil
		}

		if e.SDP != "" {
			return nil
		}

		return fmt.Errorf("mediasoup adapter requires SDP")
	default:
		// Now, if an SDP is present, we assume it's a legacy mediasoup call
		// and we don't need to validate the adapter.
		// TODO remove legacy SDP field later on - prlanzarin
		if e.SDP != "" {
			return nil
		}

		return fmt.Errorf("unsupported adapter: %s", e.Adapter)
	}

	return nil
}

func (e *StartRecording) GetSDP() string {
	// TODO remove legacy SDP field later on - prlanzarin
	if e.AdapterOptions != nil && e.AdapterOptions.Mediasoup != nil && e.AdapterOptions.Mediasoup.SDP != "" {
		return e.AdapterOptions.Mediasoup.SDP
	}
	return e.SDP
}

func (e *StartRecording) Fail(err error) *StartRecordingResponse {
	r := StartRecordingResponse{
		Id:        StartRecordingResponseKey,
		SessionId: e.SessionId,
		Status:    "failed",
		Error:     pointer.ToString(err.Error()),
	}
	return &r
}

func (e *StartRecording) Success(sdp, fileName string, metadata map[string]any) *StartRecordingResponse {
	r := StartRecordingResponse{
		Id:        StartRecordingResponseKey,
		SessionId: e.SessionId,
		Status:    "ok",
		Error:     nil,
		SDP:       pointer.ToString(sdp),
		FileName:  pointer.ToString(fileName),
		Metadata:  metadata,
	}
	return &r
}

/*
startRecordingResponse (Recorder -> SFU)
```JSON5
{
	id: 'startRecordingResponse',
	recordingSessionId: <String>, // file name,
	status: 'ok' | 'failed',
	error: undefined | <String>,
	sdp: <String | undefined>, // answer
	fileName: <String | undefined>, // full path to recording
}
```
*/

type StartRecordingResponse struct {
	Id        string         `json:"id,omitempty"`
	SessionId string         `json:"recordingSessionId,omitempty"`
	Status    string         `json:"status,omitempty"`
	Error     *string        `json:"error,omitempty"`
	SDP       *string        `json:"sdp,omitempty"`
	FileName  *string        `json:"fileName,omitempty"`
	Adapter   *string        `json:"adapter,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

func (e *StartRecordingResponse) Validate() error {
	if e.Id != StartRecordingResponseKey {
		return fmt.Errorf("invalid event id: %s", e.Id)
	}
	if e.SessionId == "" {
		return fmt.Errorf("missing session id")
	}
	if e.Status != "ok" && e.Status != "failed" {
		return fmt.Errorf("invalid status: %s", e.Status)
	}
	if e.Status == "ok" {
		if e.Error != nil {
			return fmt.Errorf("error field should not be present in success response")
		}
		if e.FileName == nil {
			return fmt.Errorf("fileName is required in success response")
		}
	} else {
		if e.Error == nil {
			return fmt.Errorf("error field is required in failure response")
		}
		if e.SDP != nil {
			return fmt.Errorf("sdp field should not be present in failure response")
		}
	}
	return nil
}

/*
recordingRtpStatusChanged (Recorder -> SFU)
```JSON5
{
	id: 'recordingRtpStatusChanged', // media started or stopped flowing
	status: 'flowing' | 'not_flowing',
	recordingSessionId: <String>, // file name
	timestampUTC: <Number>, // latest/trigger frame ts, UTC
	timestampHR: <Number>, monotonic system time (latest/trigger frame ts),
}
```
*/

type RecordingRtpStatusChanged struct {
	Id           string        `json:"id,omitempty"`
	SessionId    string        `json:"recordingSessionId,omitempty"`
	Status       string        `json:"status,omitempty"`
	TimestampUTC time.Time     `json:"timestampUTC"`
	TimestampHR  time.Duration `json:"timestampHR"`
}

var flowingStatus = map[bool]string{true: "flowing", false: "not_flowing"}

func NewRecordingRtpStatusChanged(id string, status bool, ts time.Duration) *RecordingRtpStatusChanged {
	return &RecordingRtpStatusChanged{
		Id:           RecordingRtpStatusChangedKey,
		SessionId:    id,
		Status:       flowingStatus[status],
		TimestampUTC: time.Now().UTC(),
		TimestampHR:  ts,
	}
}

/*
recordingMediaStarted (Recorder -> SFU), LiveKit sessions only
```JSON5
{
	id: 'recordingMediaStarted', // once the media start is settled, or 4.5 s old
	recordingSessionId: <String>,
	mediaStartUTC: <Number>, // when the first frame written arrived, less any backlog (at most 1 s), epoch ms
	mediaStartHR: <Number>, // the same time on the host's CLOCK_MONOTONIC, ms
}
```
*/

type RecordingMediaStarted struct {
	Id            string `json:"id,omitempty"`
	SessionId     string `json:"recordingSessionId,omitempty"`
	MediaStartUTC int64  `json:"mediaStartUTC"`
	MediaStartHR  int64  `json:"mediaStartHR"`
}

func NewRecordingMediaStarted(id string, start time.Time) *RecordingMediaStarted {
	return &RecordingMediaStarted{
		Id:            RecordingMediaStartedKey,
		SessionId:     id,
		MediaStartUTC: start.UnixMilli(),
		MediaStartHR:  MonotonicMs(start),
	}
}

/*
stopRecording (SFU -> Recorder)
```JSON5
{
	id: 'stopRecording',
	recordingSessionId: <String>, // file name
}
```
*/

type StopRecording struct {
	Id        string `json:"id,omitempty"`
	SessionId string `json:"recordingSessionId,omitempty"`
}

func (e *StopRecording) Stopped(reason string, ts time.Duration) *RecordingStopped {
	return &RecordingStopped{
		Id:           RecordingStoppedKey,
		SessionId:    e.SessionId,
		Reason:       reason,
		TimestampUTC: time.Now().UTC(),
		TimestampHR:  ts,
	}
}

/*
recordingStopped (Recorder -> SFU)
```JSON5
{
	id: 'recordingStopped',
	recordingSessionId: <String>, // file name
	reason: <String>,
  	timestampUTC: <Number>, // last written frame timestamp, UTC, wall clock
	timestampHR:  <Number> // last written frame timestamp, monotonic system time
	// LiveKit sessions that wrote a frame; see recordingMediaStarted
	mediaStartUTC?: <Number>,
	mediaStartHR?: <Number>,
	mediaEndUTC?: <Number>, // mediaStart plus the media written, to the end of the last frame
	mediaEndHR?: <Number>,
}
```
*/

type RecordingStopped struct {
	Id            string        `json:"id,omitempty"`
	SessionId     string        `json:"recordingSessionId,omitempty"`
	Reason        string        `json:"reason,omitempty"`
	TimestampUTC  time.Time     `json:"timestampUTC,omitempty"`
	TimestampHR   time.Duration `json:"timestampHR,omitempty"`
	MediaStartUTC int64         `json:"mediaStartUTC,omitempty"`
	MediaStartHR  int64         `json:"mediaStartHR,omitempty"`
	MediaEndUTC   int64         `json:"mediaEndUTC,omitempty"`
	MediaEndHR    int64         `json:"mediaEndHR,omitempty"`
}

// WithMediaSpan adds span's start and end to e and returns e.
func (e *RecordingStopped) WithMediaSpan(span types.MediaSpan) *RecordingStopped {
	e.MediaStartUTC = span.Start.UnixMilli()
	e.MediaStartHR = MonotonicMs(span.Start)
	e.MediaEndUTC = span.End.UnixMilli()
	e.MediaEndHR = MonotonicMs(span.End)

	return e
}

func NewRecordingStopped(id, reason string, ts time.Duration) *RecordingStopped {
	return &RecordingStopped{
		Id:           RecordingStoppedKey,
		SessionId:    id,
		Reason:       reason,
		TimestampUTC: time.Now().UTC(),
		TimestampHR:  ts,
	}
}

/*
recorderStatus (Recorder -> *)
```JSON5
{
	id: 'recorderStatus',
	appVersion: <String>, // version of the recorder
	instanceId: <String>, // unique instance id
	timestamp: <Number>, // event generation timestamp
	capabilities: <String[]>, // e.g. 'mediaTimestamps'
}
```
*/

type RecorderStatus struct {
	Id           string   `json:"id,omitempty"`
	AppVersion   string   `json:"appVersion,omitempty"`
	InstanceId   string   `json:"instanceId,omitempty"`
	Timestamp    int64    `json:"timestamp,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

func NewRecorderStatus(appVersion string, instanceId string) *RecorderStatus {
	return &RecorderStatus{
		Id:           RecorderStatusKey,
		AppVersion:   appVersion,
		InstanceId:   instanceId,
		Timestamp:    time.Now().UTC().UnixMilli(),
		Capabilities: []string{CapabilityMediaTimestamps},
	}
}

/*
getRecorderStatus (* -> Recorder)
```JSON5
{
	id: 'getRecorderStatus',
}
```
*/

type GetRecorderStatus struct {
	Id string `json:"id,omitempty"`
}

func (e *Event) GetRecorderStatus() *GetRecorderStatus {
	if ev, ok := e.Data.(*GetRecorderStatus); ok {
		return ev
	}
	return nil
}

func (e *GetRecorderStatus) Status(appVersion string, instanceId string) *RecorderStatus {
	return NewRecorderStatus(appVersion, instanceId)
}

/*
getRecordings (* -> Recorder)
```JSON5

	{
		"id": "getRecordings",
		"requestId": "<String>" // requester-defined - for request/response correlation
	}

```
*/
type GetRecordings struct {
	Id        string `json:"id,omitempty"`
	RequestId string `json:"requestId,omitempty"`
}

type RecordingInfo struct {
	SessionId      string          `json:"recordingSessionId,omitempty"`
	FileName       string          `json:"fileName,omitempty"`
	Adapter        AdapterType     `json:"adapter,omitempty"`
	AdapterOptions *AdapterOptions `json:"adapterOptions,omitempty"`
	Metadata       map[string]any  `json:"metadata,omitempty"`
	StartTimeUTC   *int64          `json:"startTimeUTC,omitempty"` // Unix timestamp (milliseconds)
	StartTimeHR    *time.Duration  `json:"startTimeHR,omitempty"`  // Monotonic system time (nanoseconds)
	MediaStartUTC  *int64          `json:"mediaStartUTC,omitempty"`
	MediaStartHR   *int64          `json:"mediaStartHR,omitempty"`
}

// SetMediaStart sets the media start as recordingMediaStarted sends it.
func (i *RecordingInfo) SetMediaStart(start time.Time) {
	i.MediaStartUTC = pointer.ToInt64(start.UnixMilli())
	i.MediaStartHR = pointer.ToInt64(MonotonicMs(start))
}

/*
getRecordingsResponse (Recorder -> *)
```JSON5

	{
	    "id": "getRecordingsResponse",
	    "requestId": "<String>", // Mirrors the requestId from the getRecordings request
	    "recordings": [
	        {
	            "recordingSessionId": "<String>",
	            "fileName": "<String>",
	            "adapter": "<String>", // "mediasoup" or "livekit"
	            "metadata": { ... }, // Opaque metadata from the original startRecording request
	            "adapterOptions": { ... },
	            "startTimeUTC": "<Number | undefined>", // Wall clock time (milliseconds since epoch) for the first frame
	            "startTimeHR": "<Number | undefined>", // Monotonic system time (milliseconds) for the first frame
	            "mediaStartUTC": "<Number | undefined>", // LiveKit: as in recordingMediaStarted, once sent
	            "mediaStartHR": "<Number | undefined>"
	        }
	    ]
	}

```
*/
type GetRecordingsResponse struct {
	Id         string           `json:"id,omitempty"`
	RequestId  string           `json:"requestId,omitempty"`
	Recordings []*RecordingInfo `json:"recordings"`
}

func (e *Event) GetRecordings() *GetRecordings {
	if ev, ok := e.Data.(*GetRecordings); ok {
		return ev
	}
	return nil
}
