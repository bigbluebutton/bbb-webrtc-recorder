package types

import "time"

// MediaSpan is where a recording's media sits on the wall clock.
// record-and-playback places the file's first frame at Start.
type MediaSpan struct {
	// Start is when the first frame written reached the recorder. Later
	// frames that arrived sooner than their PTS imply move it back, by at
	// most 1 s.
	Start time.Time
	// End is Start plus the media after the first frame, to the end of the
	// last frame written.
	End time.Time
	// Settled is false while the next frames written can still move Start.
	Settled bool
}
