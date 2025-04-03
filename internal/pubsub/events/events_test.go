package events

import (
	"testing"
)

func TestStartRecording_Validate(t *testing.T) {
	tests := []struct {
		name    string
		event   StartRecording
		wantErr bool
	}{
		{
			name: "valid legacy mediasoup",
			event: StartRecording{
				Id:        StartRecordingKey,
				SessionId: "test-session",
				FileName:  "test.webm",
				SDP:       "v=0\r\n...",
			},
			wantErr: false,
		},
		{
			name: "valid new mediasoup",
			event: StartRecording{
				Id:        StartRecordingKey,
				SessionId: "test-session",
				FileName:  "test.webm",
				Adapter:   AdapterMediasoup,
				AdapterOptions: &AdapterOptions{
					Mediasoup: &MediasoupConfig{
						SDP: "v=0\r\n...",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "valid livekit",
			event: StartRecording{
				Id:        StartRecordingKey,
				SessionId: "test-session",
				FileName:  "test.webm",
				Adapter:   AdapterLiveKit,
				AdapterOptions: &AdapterOptions{
					LiveKit: &LiveKitConfig{
						Room:     "test-room",
						TrackIDs: []string{"track1", "track2"},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "missing required fields",
			event: StartRecording{
				Id: StartRecordingKey,
			},
			wantErr: true,
		},
		{
			name: "invalid adapter",
			event: StartRecording{
				Id:        StartRecordingKey,
				SessionId: "test-session",
				FileName:  "test.webm",
				Adapter:   AdapterType("invalid"),
			},
			wantErr: true,
		},
		{
			name: "mediasoup without sdp",
			event: StartRecording{
				Id:        StartRecordingKey,
				SessionId: "test-session",
				FileName:  "test.webm",
				Adapter:   AdapterMediasoup,
				AdapterOptions: &AdapterOptions{
					Mediasoup: &MediasoupConfig{},
				},
			},
			wantErr: true,
		},
		{
			name: "livekit without room",
			event: StartRecording{
				Id:        StartRecordingKey,
				SessionId: "test-session",
				FileName:  "test.webm",
				Adapter:   AdapterLiveKit,
				AdapterOptions: &AdapterOptions{
					LiveKit: &LiveKitConfig{
						TrackIDs: []string{"track1"},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "livekit without trackIDs",
			event: StartRecording{
				Id:        StartRecordingKey,
				SessionId: "test-session",
				FileName:  "test.webm",
				Adapter:   AdapterLiveKit,
				AdapterOptions: &AdapterOptions{
					LiveKit: &LiveKitConfig{
						Room: "test-room",
					},
				},
			},
			wantErr: true,
		},
		{
			name: "livekit without adapterOptions",
			event: StartRecording{
				Id:        StartRecordingKey,
				SessionId: "test-session",
				FileName:  "test.webm",
				Adapter:   AdapterLiveKit,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.event.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("StartRecording.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestStartRecording_GetSDP(t *testing.T) {
	tests := []struct {
		name    string
		event   StartRecording
		wantSDP string
		wantErr bool
	}{
		{
			name: "legacy sdp",
			event: StartRecording{
				SDP: "v=0\r\nlegacy",
			},
			wantSDP: "v=0\r\nlegacy",
			wantErr: false,
		},
		{
			name: "mediasoup sdp",
			event: StartRecording{
				AdapterOptions: &AdapterOptions{
					Mediasoup: &MediasoupConfig{
						SDP: "v=0\r\nmediasoup",
					},
				},
			},
			wantSDP: "v=0\r\nmediasoup",
			wantErr: false,
		},
		{
			name: "both sdps present",
			event: StartRecording{
				SDP: "v=0\r\nlegacy",
				AdapterOptions: &AdapterOptions{
					Mediasoup: &MediasoupConfig{
						SDP: "v=0\r\nmediasoup",
					},
				},
			},
			wantSDP: "v=0\r\nmediasoup", // Should prefer mediasoup.sdp
			wantErr: false,
		},
		{
			name:    "no sdp",
			event:   StartRecording{},
			wantSDP: "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sdp := tt.event.GetSDP()
			if sdp != tt.wantSDP {
				t.Errorf("StartRecording.GetSDP() = %v, want %v", sdp, tt.wantSDP)
			}
		})
	}
}

func TestStartRecordingResponse_Validate(t *testing.T) {
	tests := []struct {
		name    string
		event   StartRecordingResponse
		wantErr bool
	}{
		{
			name: "valid success response",
			event: StartRecordingResponse{
				Id:        "startRecordingResponse",
				SessionId: "test-session",
				Status:    "ok",
				SDP:       &[]string{"v=0\r\n..."}[0],
				FileName:  &[]string{"test.webm"}[0],
			},
			wantErr: false,
		},
		{
			name: "valid failure response",
			event: StartRecordingResponse{
				Id:        "startRecordingResponse",
				SessionId: "test-session",
				Status:    "failed",
				Error:     &[]string{"test error"}[0],
			},
			wantErr: false,
		},
		{
			name: "missing required fields",
			event: StartRecordingResponse{
				Id: "startRecordingResponse",
			},
			wantErr: true,
		},
		{
			name: "invalid status",
			event: StartRecordingResponse{
				Id:        "startRecordingResponse",
				SessionId: "test-session",
				Status:    "invalid",
			},
			wantErr: true,
		},
		{
			name: "success without sdp",
			event: StartRecordingResponse{
				Id:        "startRecordingResponse",
				SessionId: "test-session",
				Status:    "ok",
				FileName:  &[]string{"test.webm"}[0],
			},
			wantErr: false,
		},
		{
			name: "failure with sdp",
			event: StartRecordingResponse{
				Id:        "startRecordingResponse",
				SessionId: "test-session",
				Status:    "failed",
				SDP:       &[]string{"v=0\r\n..."}[0],
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.event.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("StartRecordingResponse.Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
