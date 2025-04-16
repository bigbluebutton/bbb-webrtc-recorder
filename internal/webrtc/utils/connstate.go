package utils

import (
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v3"
)

// ConnectionState represents a normalized connection state across different
// media/WebRTC adapters
type ConnectionState int

const (
	ConnectionStateNew ConnectionState = iota
	ConnectionStateConnecting
	ConnectionStateConnected
	ConnectionStateDisconnected
	ConnectionStateFailed
	ConnectionStateClosed
)

// IsTerminalState returns true if the connection state is terminal (failed or closed)
func (s ConnectionState) IsTerminalState() bool {
	return s == ConnectionStateFailed || s == ConnectionStateClosed
}

// NormalizeWebRTCState converts a pion/webrtc ICEConnectionState to ConnectionState
func NormalizeWebRTCState(state webrtc.ICEConnectionState) ConnectionState {
	switch state {
	case webrtc.ICEConnectionStateNew:
		return ConnectionStateNew
	case webrtc.ICEConnectionStateChecking:
		return ConnectionStateConnecting
	case webrtc.ICEConnectionStateConnected:
		return ConnectionStateConnected
	case webrtc.ICEConnectionStateCompleted:
		return ConnectionStateConnected
	case webrtc.ICEConnectionStateDisconnected:
		return ConnectionStateDisconnected
	case webrtc.ICEConnectionStateFailed:
		return ConnectionStateFailed
	case webrtc.ICEConnectionStateClosed:
		return ConnectionStateClosed
	default:
		return ConnectionStateNew
	}
}

// NormalizeLiveKitDisconnectReason converts a LiveKit DisconnectionReason to ConnectionState
func NormalizeLiveKitDisconnectReason(reason lksdk.DisconnectionReason) ConnectionState {
	switch reason {
	case lksdk.LeaveRequested,
		lksdk.UserUnavailable,
		lksdk.RoomClosed,
		lksdk.ParticipantRemoved:
		return ConnectionStateClosed
	case lksdk.RejectedByUser,
		lksdk.Failed,
		lksdk.DuplicateIdentity:
		return ConnectionStateFailed
	default:
		return ConnectionStateFailed
	}
}

func (s ConnectionState) String() string {
	switch s {
	case ConnectionStateNew:
		return "new"
	case ConnectionStateConnecting:
		return "connecting"
	case ConnectionStateConnected:
		return "connected"
	case ConnectionStateDisconnected:
		return "disconnected"
	case ConnectionStateFailed:
		return "failed"
	case ConnectionStateClosed:
		return "closed"
	default:
		return "unknown"
	}
}
