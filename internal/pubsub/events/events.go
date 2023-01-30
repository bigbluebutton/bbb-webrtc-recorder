package events

import "time"

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

//type StartRecording struct {
//	Id                 string  `json:",omitempty"`
//	RecordingSessionId string  `json:",omitempty"`
//	SDP                *string `json:",omitempty"`
//	FileName           string  `json:",omitempty"`
//}

type StartRecording struct {
	Id                 string `json:"id,omitempty"`
	RecordingSessionId string `json:"recordingSessionId,omitempty"`
	SDP                string `json:"sdp,omitempty"`
	FileName           string `json:"fileName,omitempty"`
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
	Id                 string  `json:"id,omitempty"`
	RecordingSessionId string  `json:"recordingSessionId,omitempty"`
	Status             string  `json:"status,omitempty"`
	Error              *string `json:"error,omitempty"`
	SDP                *string `json:"sdp,omitempty"`
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
	Id                 string        `json:"id,omitempty"`
	RecordingSessionId string        `json:"recordingSessionId,omitempty"`
	Status             string        `json:"status,omitempty"`
	TimestampUTC       time.Time     `json:"timestampUTC"`
	TimestampHR        time.Duration `json:"timestampHR"`
}

var FlowingStatus = map[bool]string{true: "flowing", false: "not_flowing"}

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
	Id                 string `json:"id,omitempty"`
	RecordingSessionId string `json:"recordingSessionId,omitempty"`
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
	Id                 string    `json:"id,omitempty"`
	RecordingSessionId string    `json:"recordingSessionId,omitempty"`
	Reason             string    `json:"reason,omitempty"`
	TimestampUTC       time.Time `json:"timestampUTC,omitempty"`
	TimestampHR        time.Time `json:"timestampHR,omitempty"`
}
