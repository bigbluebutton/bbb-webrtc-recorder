package events

import (
	"github.com/titanous/json5"
)

func Decode(message []byte) (string, interface{}) {
	m := make(map[string]interface{})
	if err := json5.Unmarshal(message, &m); err != nil {

	}

	if _, ok := m["id"].(string); !ok {

	}
	id := m["id"].(string)

	var r interface{}
	switch id {
	case "startRecording":
		s := StartRecording{}
		if err := json5.Unmarshal(message, &s); err != nil {

		}
		r = s
	case "stopRecording":
		s := StopRecording{}
		if err := json5.Unmarshal(message, &s); err != nil {

		}
		r = s
	case "startRecordingResponse":
		s := StartRecordingResponse{}
		if err := json5.Unmarshal(message, &s); err != nil {

		}
		r = s
	default:

	}

	return id, r
}
