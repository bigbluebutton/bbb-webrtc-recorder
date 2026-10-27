package types

import "time"

type DiscontinuityInfo struct {
	Count    int    `json:"count"`
	MinGap   uint16 `json:"minGap"`
	MaxGap   uint16 `json:"maxGap"`
	AvgGap   uint16 `json:"avgGap"`
	TotalGap uint16 `json:"totalGap"`
}

type BaseTrackStats struct {
	StartTime           int64             `json:"startTime"`
	EndTime             int64             `json:"endTime"`
	StartPTS            int64             `json:"startPts"`
	EndPTS              int64             `json:"endPts"`
	AvgSampleDurationMs time.Duration     `json:"avgSampleDurationMs"`
	MaxSampleDurationMs time.Duration     `json:"maxSampleDurationMs"`
	TotalSamples        int               `json:"totalSamples"`
	WrittenSamples      int               `json:"writtenSamples"`
	RTPDiscontInfo      DiscontinuityInfo `json:"rtpDiscontInfo"`

	// sampleDurationAcc is an internal accumulator and should not be marshaled.
	SampleDurationAcc time.Duration `json:"-"`
}

type RecorderTrackStats struct {
	BaseTrackStats
	CorruptedFrames     int               `json:"corruptedFrames,omitempty"`
	AvgFrameSizeBytes   int               `json:"avgFrameSizeBytes,omitempty"`
	MaxFrameSizeBytes   int               `json:"maxFrameSizeBytes,omitempty"`
	KeyframeCount       int               `json:"keyframeCount,omitempty"`
	VP8PicIDDiscontInfo DiscontinuityInfo `json:"vp8PicIdDiscontInfo,omitempty"`
}

type RecorderStats struct {
	Audio *RecorderTrackStats `json:"audio,omitempty"`
	Video *RecorderTrackStats `json:"video,omitempty"`
}

// MediaClockStats describes a stream's media clock against the wall clock.
type MediaClockStats struct {
	// RTPDurationMs is the media time between the first and the newest
	// timestamp; ArrivalDurationMs the time between their arrivals.
	// RateRatio is their ratio, less what LiveKit realigned: below 1 when the
	// sender's media clock runs slow.
	RTPDurationMs     int64   `json:"rtpDurationMs"`
	ArrivalDurationMs int64   `json:"arrivalDurationMs"`
	RateRatio         float64 `json:"rateRatio"`
	// SenderReports counts the reports used; SenderReportsIgnored those
	// repeated or outside the stream's timestamps. SenderClockSteps counts
	// the reports whose NTP or RTP time stepped; they add no loss.
	SenderReports        int `json:"senderReports"`
	SenderReportsIgnored int `json:"senderReportsIgnored"`
	SenderClockSteps     int `json:"senderClockSteps"`
	// ReportedDriftMs is the sender's media time less its wall time since the
	// first packet, by its sender reports; LiveKit's realignments do not
	// reduce it. Negative when the media clock runs slow, as LiveKit's
	// RTPDrift.driftMs.
	ReportedDriftMs int64 `json:"reportedDriftMs"`
	// InsertedGaps counts the timestamp gaps inserted, InsertedGapMs their
	// total. Realignments counts LiveKit's jumps of the timestamps to the
	// wall clock that the next reports confirmed, RealignedMs their total.
	// GivenBackMs is the gap time taken back where the timestamps jumped
	// ahead of their arrival.
	InsertedGaps  int   `json:"insertedGaps"`
	InsertedGapMs int64 `json:"insertedGapMs"`
	Realignments  int   `json:"realignments"`
	RealignedMs   int64 `json:"realignedMs"`
	GivenBackMs   int64 `json:"givenBackMs"`
	// LagMs is how far the newest packet arrived behind its corrected place;
	// MaxLagMs the most it was. A network delay that grows adds to both.
	LagMs    int64 `json:"lagMs"`
	MaxLagMs int64 `json:"maxLagMs"`
}
