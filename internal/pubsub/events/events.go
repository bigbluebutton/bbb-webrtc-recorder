package events

import (
	"github.com/AlekSi/pointer"
	"time"
)

type Event struct {
	Id   string
	Data interface{}
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
	id: ‘startRecording’,
	recordingSessionId: <String> // requester-defined - error out if collision.
	sdp: <String>, // offer
	fileName: <String>, // file name INCLUDING format (.webm)
}
```
*/

type StartRecording struct {
	Id        string `json:"id,omitempty"`
	SessionId string `json:"recordingSessionId,omitempty"`
	SDP       string `json:"sdp,omitempty"`
	FileName  string `json:"fileName,omitempty"`
}

func (e *StartRecording) Fail(err error) *StartRecordingResponse {
	r := StartRecordingResponse{
		Id:        "startRecordingResponse",
		SessionId: e.SessionId,
		Status:    "failed",
		Error:     pointer.ToString(err.Error()),
	}
	return &r
}

func (e *StartRecording) Success(sdp string) *StartRecordingResponse {
	r := StartRecordingResponse{
		Id:        "startRecordingResponse",
		SessionId: e.SessionId,
		Status:    "ok",
		Error:     nil,
		SDP:       pointer.ToString(sdp),
	}
	return &r
}

/*
startRecordingResponse (Recorder -> SFU)
```JSON5
{
	id: ‘startRecordingResponse’,
	recordingSessionId: <String>, // file name,
	status: ‘ok’ | ‘failed’,
	error: undefined | <String>,
	sdp: <String | undefined>, // answer
}
```
*/

type StartRecordingResponse struct {
	Id        string  `json:"id,omitempty"`
	SessionId string  `json:"recordingSessionId,omitempty"`
	Status    string  `json:"status,omitempty"`
	Error     *string `json:"error,omitempty"`
	SDP       *string `json:"sdp,omitempty"`
}

/*
recordingRtpStatusChanged (Recorder -> SFU)
```JSON5
{
	id: ‘recordingRtpStatusChanged’, // media started or stopped flowing
	status: ‘flowing’ | ‘not_flowing’,
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
		Id:           "recordingRtpStatusChanged",
		SessionId:    id,
		Status:       flowingStatus[status],
		TimestampUTC: time.Now().UTC(),
		TimestampHR:  ts,
	}
}

/*
stopRecording (SFU -> Recorder)
```JSON5
{
	id: ‘stopRecording’,
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
		Id:           "recordingStopped",
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
	id: ‘recordingStopped’,
	recordingSessionId: <String>, // file name
	reason: <String>,
  	timestampUTC: <Number>, // last written frame timestamp, UTC, wall clock
	timestampHR:  <Number> // last written frame timestamp, monotonic system time
}
```
*/

type RecordingStopped struct {
	Id           string        `json:"id,omitempty"`
	SessionId    string        `json:"recordingSessionId,omitempty"`
	Reason       string        `json:"reason,omitempty"`
	TimestampUTC time.Time     `json:"timestampUTC,omitempty"`
	TimestampHR  time.Duration `json:"timestampHR,omitempty"`
}

func NewRecordingStopped(id, reason string, ts time.Duration) *RecordingStopped {
	return &RecordingStopped{
		Id:           "recordingStopped",
		SessionId:    id,
		Reason:       reason,
		TimestampUTC: time.Now().UTC(),
		TimestampHR:  ts,
	}
}
