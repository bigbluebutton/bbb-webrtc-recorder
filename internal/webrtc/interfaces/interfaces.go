package interfaces

import "time"

type KeyframeRequester interface {
	RequestKeyframe()
	RequestKeyframeForSSRC(ssrc uint32)
}

// KeyframeRequestTracker is a KeyframeRequester that also requests key
// frames on its own, for losses it detects before the writer does.
type KeyframeRequestTracker interface {
	KeyframeRequester

	// KeyframeRequestedSince reports whether a key frame it requested for
	// ssrc after since is still due.
	KeyframeRequestedSince(ssrc uint32, since time.Time) bool
}
