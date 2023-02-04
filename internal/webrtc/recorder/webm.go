package recorder

import (
	"context"
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
	ctx  context.Context
	file string

	audioWriter, videoWriter       webm.BlockWriteCloser
	audioBuilder, videoBuilder     *samplebuilder.SampleBuilder
	audioTimestamp, videoTimestamp time.Duration

	jitterBuffer *utils.JitterBuffer
	seqUnwrapper *utils.SequenceUnwrapper

	started bool
	flowing bool
	closed  bool

	flowCallbackFn   FlowCallbackFn
	keyframeSequence int64

	statusTicker     *time.Ticker
	statusTickerChan chan bool
}

func NewWebmRecorder(file string, fn FlowCallbackFn) *WebmRecorder {
	r := &WebmRecorder{
		ctx:            context.Background(),
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
				if ts == r.videoTimestamp {
					r.flowing = false
					r.flowCallbackFn(r.flowing, r.keyframeSequence, r.videoTimestamp, r.closed)
				}
			}
			ts = r.videoTimestamp
		}
	}()

	return r
}

func (r *WebmRecorder) SetContext(ctx context.Context) {
	r.ctx = ctx
}

func (r *WebmRecorder) Push(p *rtp.Packet, track *webrtc.TrackRemote) {
	if r.closed {
		return
	}

	seq := r.seqUnwrapper.Unwrap(uint64(p.SequenceNumber))
	if !r.jitterBuffer.Add(seq, p) {
		return
	}

	for _, p := range r.jitterBuffer.NextPackets() {
		switch track.Kind() {
		case webrtc.RTPCodecTypeAudio:
			r.pushOpus(p)
		case webrtc.RTPCodecTypeVideo:
			r.pushVP8(p)
		}
	}
}

func (r *WebmRecorder) Close() time.Duration {
	if r.closed {
		return r.videoTimestamp
	}
	r.flowing = false
	r.closed = true
	r.statusTicker.Stop()
	r.statusTickerChan <- true

	if r.audioWriter != nil {
		if err := r.audioWriter.Close(); err != nil {
			panic(err)
		}
	}
	if r.videoWriter != nil {
		if err := r.videoWriter.Close(); err != nil {
			panic(err)
		}
	}
	if r.started {
		//r.flowCallbackFn(r.flowing, r.keyframeSequence, r.videoTimestamp, r.closed)
		log.WithField("session", r.ctx.Value("session")).
			Printf("webm writer closed: %s", r.file)
	} else {
		log.WithField("session", r.ctx.Value("session")).
			Print("webm writer closed without starting")
	}
	return r.videoTimestamp
}

func (r *WebmRecorder) pushOpus(rtpPacket *rtp.Packet) {
	r.audioBuilder.Push(rtpPacket)

	for {
		sample := r.audioBuilder.Pop()
		if sample == nil {
			return
		}
		if r.audioWriter != nil {
			r.audioTimestamp += sample.Duration
			if _, err := r.audioWriter.Write(true, int64(r.audioTimestamp/time.Millisecond), sample.Data); err != nil {
				panic(err)
			}
		}
	}
}

func (r *WebmRecorder) pushVP8(rtpPacket *rtp.Packet) {
	r.videoBuilder.Push(rtpPacket)

	var ts time.Duration
	for {
		sample := r.videoBuilder.Pop()
		if sample == nil {
			return
		}
		// Read VP8 header.
		videoKeyframe := sample.Data[0]&0x1 == 0
		if videoKeyframe {
			// jitter buffer content will be dropped up until the keyframe
			seq := r.seqUnwrapper.Unwrap(uint64(rtpPacket.SequenceNumber))
			r.jitterBuffer.SetNextPacketsStart(seq)
			r.keyframeSequence = seq

			//log.Debug("keyframe #", seq)

			// Keyframe has frame information.
			raw := uint(sample.Data[6]) | uint(sample.Data[7])<<8 | uint(sample.Data[8])<<16 | uint(sample.Data[9])<<24
			width := int(raw & 0x3FFF)
			height := int((raw >> 16) & 0x3FFF)

			if r.videoWriter == nil || r.audioWriter == nil {
				//Initialize WebM saver using received frame size.
				r.initWriter(width, height)
			}
		}
		if r.videoWriter != nil {
			r.videoTimestamp += sample.Duration
			if r.closed {
				break
			}
			if _, err := r.videoWriter.Write(videoKeyframe, int64(r.videoTimestamp/time.Millisecond), sample.Data); err != nil {
				panic(err)
			}
		}
		if ts == r.videoTimestamp {
			r.flowing = false
		} else {
			ts = r.videoTimestamp
		}
		if !r.flowing || videoKeyframe {
			r.flowCallbackFn(r.flowing, r.keyframeSequence, r.videoTimestamp, r.closed)
		}

	}
}

func (r *WebmRecorder) initWriter(width, height int) {
	w, err := os.OpenFile(r.file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
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
	log.WithField("session", r.ctx.Value("session")).
		Printf("webm writer started with %dx%d video: %s", width, height, r.file)
	r.audioWriter = ws[0]
	r.videoWriter = ws[1]
	r.started = true

	r.flowing = true
}
