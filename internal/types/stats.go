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
