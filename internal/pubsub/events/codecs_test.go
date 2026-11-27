package events

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartRecordingSuccessReportsTheCodecs(t *testing.T) {
	e := &StartRecording{SessionId: "s1"}
	r := e.Success("", "/rec/x.webm", []string{"video/vp8"}, nil)
	require.NoError(t, r.Validate())

	data, err := json.Marshal(r)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, "/rec/x.webm", got["fileName"])
	assert.Equal(t, []any{"video/vp8"}, got["codecs"])
}

func TestRecordingInfoReportsTheCodecs(t *testing.T) {
	data, err := json.Marshal(&RecordingInfo{SessionId: "s1", FileName: "/rec/x.webm", Codecs: []string{"video/vp8", "audio/opus"}})
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, []any{"video/vp8", "audio/opus"}, got["codecs"])
}
