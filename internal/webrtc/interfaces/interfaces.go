package interfaces

// KeyframeRequester defines the interface for requesting keyframes
type KeyframeRequester interface {
	RequestKeyframe()
	RequestKeyframeForSSRC(ssrc uint32)
}
