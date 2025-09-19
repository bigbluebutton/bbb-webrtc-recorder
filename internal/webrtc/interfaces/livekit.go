package interfaces

import (
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/utils"
)

// LiveKitWebRTCInterface is an interface wrapper for mocking
type LiveKitWebRTCInterface interface {
	SetConnectionStateCallback(callback func(state utils.ConnectionState))
	SetFlowCallback(callback func(isFlowing bool, timestamp time.Duration, closed bool))
	Init() error
	Close() time.Duration
	GetStats() *appstats.CaptureStats
	RequestKeyframe()
	RequestKeyframeForSSRC(ssrc uint32)
	HasTrack(trackID string) bool
}
