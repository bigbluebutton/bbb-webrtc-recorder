package events

import (
	log "github.com/sirupsen/logrus"
	"github.com/titanous/json5"
)

func Decode(message []byte) *Event {
	e := &Event{}
	m := make(map[string]interface{})
	if err := json5.Unmarshal(message, &m); err != nil {
		log.Error(err, message)
		return nil
	}

	var id string
	var ok bool
	if id, ok = m["id"].(string); !ok {
		return nil
	}

	var s interface{}
	switch id {
	case "startRecording":
		s = &StartRecording{}
	case "stopRecording":
		s = &StopRecording{}
	case "startRecordingResponse":
		s = &StartRecordingResponse{}
	case "recordingRtpStatusChanged":
		s = &RecordingRtpStatusChanged{}
	case "getRecorderStatus":
		s = &GetRecorderStatus{}
	case "recorderStatus":
		s = &RecorderStatus{}
	default:
		var v map[string]interface{}
		s = &v
	}

	if err := json5.Unmarshal(message, s); err != nil {
		log.Error(err, string(message))
		return nil
	}

	e.Id = id
	e.Data = s

	return e
}
