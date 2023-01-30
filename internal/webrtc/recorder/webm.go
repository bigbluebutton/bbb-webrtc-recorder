package recorder

import (
	"github.com/at-wat/ebml-go/mkvcore"
	"github.com/at-wat/ebml-go/webm"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/utils"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media/samplebuilder"
	log "github.com/sirupsen/logrus"
	"os"
	"time"
)

var _ Recorder = (*WebmRecorder)(nil)

type WebmRecorder struct {
	file string

	audioWriter, videoWriter       webm.BlockWriteCloser
	audioBuilder, videoBuilder     *samplebuilder.SampleBuilder
	audioTimestamp, videoTimestamp time.Duration

	jitterBuffer *utils.JitterBuffer
	seqUnwrapper *utils.SequenceUnwrapper

	flowing bool
	closed  bool

	flowCallbackFn   FlowCallbackFn
	keyframeSequence int64

	statusTicker     *time.Ticker
	statusTickerChan chan bool
}

func NewWebmRecorder(file string, fn FlowCallbackFn) *WebmRecorder {
	r := &WebmRecorder{
		file:           file,
		audioBuilder:   samplebuilder.New(10, &codecs.OpusPacket{}, 48000),
		videoBuilder:   samplebuilder.New(10, &codecs.VP8Packet{}, 90000),
		seqUnwrapper:   utils.NewSequenceUnwrapper(16),
		jitterBuffer:   utils.NewJitterBuffer(512),
		flowCallbackFn: fn,
	}

	r.statusTicker = time.NewTicker(1000 * time.Millisecond)
	r.statusTickerChan = make(chan bool)
	go func() {
		var ts time.Duration
		for {
			select {
			case <-r.statusTickerChan:
				return
			case <-r.statusTicker.C:
				//if !r.flowing || ts == r.videoTimestamp {
				if ts == r.videoTimestamp {
					r.flowing = false
					r.flowCallbackFn(r.flowing, r.keyframeSequence, r.videoTimestamp)
				}
			}
			ts = r.videoTimestamp
		}
	}()

	return r
}

func (s *WebmRecorder) Push(r *rtp.Packet, track *webrtc.TrackRemote) {
	if s.closed {
		return
	}

	seq := s.seqUnwrapper.Unwrap(uint64(r.SequenceNumber))
	if !s.jitterBuffer.Add(seq, r) {
		return
	}

	// jitterBuffer.SetNextPacketsStart() can be called in case of a keyframe, so the buffer content is dropped up until the keyframe

	for _, r := range s.jitterBuffer.NextPackets() {
		switch track.Kind() {
		case webrtc.RTPCodecTypeAudio:
			s.PushOpus(r)
		case webrtc.RTPCodecTypeVideo:
			s.PushVP8(r)
		}
	}
}

func (s *WebmRecorder) Close() {
	if s.closed {
		return
	}
	s.flowing = false
	s.closed = true
	s.statusTicker.Stop()
	s.statusTickerChan <- true
	if s.audioWriter != nil {
		if err := s.audioWriter.Close(); err != nil {
			panic(err)
		}
	}
	if s.videoWriter != nil {
		if err := s.videoWriter.Close(); err != nil {
			panic(err)
		}
	}
	log.Printf("Closing %s", s.file)
}

func (s *WebmRecorder) PushOpus(rtpPacket *rtp.Packet) {
	s.audioBuilder.Push(rtpPacket)

	for {
		sample := s.audioBuilder.Pop()
		if sample == nil {
			return
		}
		if s.audioWriter != nil {
			s.audioTimestamp += sample.Duration
			if _, err := s.audioWriter.Write(true, int64(s.audioTimestamp/time.Millisecond), sample.Data); err != nil {
				panic(err)
			}
		}
	}
}

func (s *WebmRecorder) PushVP8(rtpPacket *rtp.Packet) {
	s.videoBuilder.Push(rtpPacket)

	var ts time.Duration
	for {
		sample := s.videoBuilder.Pop()
		if sample == nil {
			return
		}
		// Read VP8 header.
		videoKeyframe := sample.Data[0]&0x1 == 0
		if videoKeyframe {
			// jitter buffer content will be dropped up until the keyframe
			seq := s.seqUnwrapper.Unwrap(uint64(rtpPacket.SequenceNumber))
			s.jitterBuffer.SetNextPacketsStart(seq)
			s.keyframeSequence = seq

			//log.Debug("keyframe #", seq)

			// Keyframe has frame information.
			raw := uint(sample.Data[6]) | uint(sample.Data[7])<<8 | uint(sample.Data[8])<<16 | uint(sample.Data[9])<<24
			width := int(raw & 0x3FFF)
			height := int((raw >> 16) & 0x3FFF)

			if s.videoWriter == nil || s.audioWriter == nil {
				//Initialize WebM saver using received frame size.
				s.InitWriter(width, height)
			}
		}
		if s.videoWriter != nil {
			s.videoTimestamp += sample.Duration
			if s.closed {
				break
			}
			if _, err := s.videoWriter.Write(videoKeyframe, int64(s.videoTimestamp/time.Millisecond), sample.Data); err != nil {
				log.Error(err)
				return
			}
		}
		if ts == s.videoTimestamp {
			s.flowing = false
		} else {
			ts = s.videoTimestamp
		}
		if !s.flowing || videoKeyframe {
			s.flowCallbackFn(s.flowing, s.keyframeSequence, s.videoTimestamp)
		}

	}
}
func (s *WebmRecorder) InitWriter(width, height int) {
	w, err := os.OpenFile(s.file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		panic(err)
	}

	info := &webm.Info{
		TimecodeScale: 1000000, // 1ms
		MuxingApp:     internal.AppName,
		WritingApp:    internal.AppName,
	}

	ws, err := webm.NewSimpleBlockWriter(w,
		[]webm.TrackEntry{
			{
				Name:            "Audio",
				TrackNumber:     1,
				TrackUID:        12345,
				CodecID:         "A_OPUS",
				TrackType:       2,
				DefaultDuration: 20000000,
				Audio: &webm.Audio{
					SamplingFrequency: 48000.0,
					Channels:          2,
				},
			}, {
				Name:            "Video",
				TrackNumber:     2,
				TrackUID:        67890,
				CodecID:         "V_VP8",
				TrackType:       1,
				DefaultDuration: 33333333,
				Video: &webm.Video{
					PixelWidth:  uint64(width),
					PixelHeight: uint64(height),
				},
			},
		}, mkvcore.WithSegmentInfo(info))
	if err != nil {
		panic(err)
	}
	log.Printf("WebM saver has started with video width=%d, height=%d\n", width, height)
	s.audioWriter = ws[0]
	s.videoWriter = ws[1]

	s.flowing = true
}
