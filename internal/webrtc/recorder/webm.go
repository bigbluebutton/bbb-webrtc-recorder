package recorder

import (
	"context"
	"github.com/at-wat/ebml-go/mkvcore"
	"github.com/at-wat/ebml-go/webm"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3/pkg/media/samplebuilder"
	log "github.com/sirupsen/logrus"
	"os"
	"time"
)

var _ Recorder = (*WebmRecorder)(nil)

const (
	vp8SampleRate       = 90000
	secondToNanoseconds = 1000000000
)

type WebmRecorder struct {
	ctx  context.Context
	file string

	audioWriter, videoWriter       webm.BlockWriteCloser
	audioBuilder, videoBuilder     *samplebuilder.SampleBuilder
	audioTimestamp, videoTimestamp time.Duration

	started bool
	closed  bool

	seenKeyFrame    bool
	currentFrame    []byte
	packetTimestamp uint32
	hasAudio        bool
}

func NewWebmRecorder(file string) *WebmRecorder {
	r := &WebmRecorder{
		ctx:          context.Background(),
		file:         file,
		audioBuilder: samplebuilder.New(10, &codecs.OpusPacket{}, 48000),
	}

	return r
}

func (r *WebmRecorder) GetFilePath() string {
	return r.file
}

func (r *WebmRecorder) WithContext(ctx context.Context) {
	r.ctx = ctx
}

func (r *WebmRecorder) SetHasAudio(hasAudio bool) {
	r.hasAudio = hasAudio
}

func (r *WebmRecorder) GetHasAudio() bool {
	return r.hasAudio
}

func (r *WebmRecorder) VideoTimestamp() time.Duration {
	return r.videoTimestamp
}

func (r *WebmRecorder) AudioTimestamp() time.Duration {
	return r.audioTimestamp
}

func (r *WebmRecorder) PushVideo(p *rtp.Packet) {
	r.pushVP8(p)
}

func (r *WebmRecorder) PushAudio(p *rtp.Packet) {
	r.pushOpus(p)
}

func (r *WebmRecorder) Close() time.Duration {
	if r.closed {
		return r.videoTimestamp
	}
	r.closed = true

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
		log.WithField("session", r.ctx.Value("session")).
			Printf("webm writer closed: %s", r.file)
	} else {
		log.WithField("session", r.ctx.Value("session")).
			Print("webm writer closed without starting")
	}
	return r.videoTimestamp
}

func (r *WebmRecorder) pushOpus(p *rtp.Packet) {
	r.audioBuilder.Push(p)

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

func (r *WebmRecorder) pushVP8(p *rtp.Packet) {
	if len(p.Payload) == 0 || r.closed {
		return
	}

	vp8Packet := codecs.VP8Packet{}
	if _, err := vp8Packet.Unmarshal(p.Payload); err != nil {
		return
	}

	isKeyFrame := vp8Packet.Payload[0]&0x01 == 0

	switch {
	case !r.seenKeyFrame && !isKeyFrame:
		return
	case r.currentFrame == nil && vp8Packet.S != 1:
		return
	}

	if !r.seenKeyFrame {
		r.packetTimestamp = p.Timestamp
	}

	r.seenKeyFrame = true
	r.currentFrame = append(r.currentFrame, vp8Packet.Payload[0:]...)

	if !p.Marker || len(r.currentFrame) == 0 {
		return
	}

	// from github.com/pion/webrtc/v3@v3.1.56/pkg/media/samplebuilder/samplebuilder.go#264
	duration := time.Duration((float64(p.Timestamp-r.packetTimestamp)/float64(vp8SampleRate))*secondToNanoseconds) * time.Nanosecond
	if r.videoWriter == nil || (r.audioWriter == nil && r.hasAudio) {
		raw := uint(r.currentFrame[6]) | uint(r.currentFrame[7])<<8 | uint(r.currentFrame[8])<<16 | uint(r.currentFrame[9])<<24
		width := int(raw & 0x3FFF)
		height := int((raw >> 16) & 0x3FFF)
		r.initWriter(width, height)
	}
	if r.videoWriter != nil {
		r.videoTimestamp += duration
		if _, err := r.videoWriter.Write(isKeyFrame, int64(r.videoTimestamp/time.Millisecond), r.currentFrame); err != nil {
			log.Error(err)
		}
	}

	r.currentFrame = nil
	r.packetTimestamp = p.Timestamp
}

func (r *WebmRecorder) initWriter(width, height int) {
	w, err := os.OpenFile(r.file, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0700)
	if err != nil {
		panic(err)
	}

	info := &webm.Info{
		TimecodeScale: 1000000, // 1ms
		MuxingApp:     internal.AppName,
		WritingApp:    internal.AppName,
	}

	// TODO make this dynamic based on the tracks we have in the peer connection
	// when we allow for multiple tracks and/or multiple codecs

	// Video track is always present - initialize tracks a array with it
	tracks := []webm.TrackEntry{
		{
			Name:        "Video",
			TrackNumber: 1,
			TrackUID:    12345,
			CodecID:     "V_VP8",
			TrackType:   1,
			Video: &webm.Video{
				PixelWidth:  uint64(width),
				PixelHeight: uint64(height),
			},
		},
	}

	// Audio track is optional
	if r.hasAudio {
		tracks = append(tracks, webm.TrackEntry{
			Name:        "Audio",
			TrackNumber: 2,
			TrackUID:    54321,
			CodecID:     "A_OPUS",
			TrackType:   2,
			Audio: &webm.Audio{
				SamplingFrequency: 48000.0,
				Channels:          2,
			},
		})
	}

	writers, err := webm.NewSimpleBlockWriter(w, tracks, mkvcore.WithSegmentInfo(info))

	if err != nil {
		panic(err)
	}
	log.WithField("session", r.ctx.Value("session")).
		Printf("webm writers started with %dx%d video, audio=%t : %s", width, height, r.hasAudio, r.file)
	r.videoWriter = writers[0]
	if r.hasAudio && len(writers) > 1 {
		r.audioWriter = writers[1]
	}
	r.started = true
}
