package types

import "github.com/livekit/protocol/livekit"

// ConstraintReasonDiskPressure: low free space in the recordings filesystem.
const ConstraintReasonDiskPressure = "disk_pressure"

// CaptureConstraint caps the video quality of the capture tracks whose
// source is listed in Sources. Reason identifies the constraint source.
// A capture holds at most one constraint per reason, and the lowest cap
// among the active ones wins.
type CaptureConstraint struct {
	Reason       string
	VideoQuality livekit.VideoQuality
	Sources      []livekit.TrackSource
}
