package interfaces

type KeyframeRequester interface {
	RequestKeyframe()
	RequestKeyframeForSSRC(ssrc uint32)
}
