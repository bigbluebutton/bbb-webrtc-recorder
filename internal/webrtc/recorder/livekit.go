package recorder

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/rtp"
	log "github.com/sirupsen/logrus"
)

var _ Recorder = (*LiveKitRecorder)(nil)

type LiveKitRecorder struct {
	m            sync.Mutex
	ctx          context.Context
	cfg          config.LiveKit
	file         string
	fileMode     os.FileMode
	room         *lksdk.Room
	tracks       map[string]*lksdk.Track
	webmRecorder *WebmRecorder
	hasAudio     bool
	hasVideo     bool
	closed       bool
	webrtc       *livekit.LiveKitWebRTC
}

func NewLiveKitRecorder(
	ctx context.Context,
	cfg config.LiveKit,
	file string,
	fileMode os.FileMode,
	videoPacketQueueSize uint16,
	audioPacketQueueSize uint16,
	useCustomSampler bool,
	writeIVFCopy bool,
) (*LiveKitRecorder, error) {
	r := &LiveKitRecorder{
		ctx:      ctx,
		cfg:      cfg,
		file:     file,
		fileMode: fileMode,
		tracks:   make(map[string]*lksdk.Track),
		webmRecorder: NewWebmRecorder(
			file,
			fileMode,
			videoPacketQueueSize,
			audioPacketQueueSize,
			useCustomSampler,
			writeIVFCopy,
		),
	}

	r.webrtc = livekit.NewLiveKitWebRTC(ctx, cfg, r)

	return r, nil
}

func (r *LiveKitRecorder) GetFilePath() string {
	return r.file
}

func (r *LiveKitRecorder) WithContext(ctx context.Context) {
	r.ctx = ctx
	r.webmRecorder.WithContext(ctx)
}

func (r *LiveKitRecorder) Connect(room string, trackIDs []string) error {
	return r.webrtc.Connect(room, trackIDs)
}

func (r *LiveKitRecorder) Close() time.Duration {
	r.m.Lock()
	defer r.m.Unlock()

	if r.closed {
		return r.VideoTimestamp()
	}
	r.closed = true

	if r.room != nil {
		r.room.Disconnect()
	}

	if err := r.webrtc.Close(); err != nil {
		log.WithField("session", r.ctx.Value("session")).
			Errorf("Error closing WebRTC handler: %v", err)
	}

	return r.webmRecorder.Close()
}

// Recorder downstream methods

func (r *LiveKitRecorder) SetHasAudio(hasAudio bool) {
	r.hasAudio = hasAudio
	r.webmRecorder.SetHasAudio(hasAudio)
}

func (r *LiveKitRecorder) GetHasAudio() bool {
	return r.hasAudio
}

func (r *LiveKitRecorder) SetHasVideo(hasVideo bool) {
	r.hasVideo = hasVideo
	r.webmRecorder.SetHasVideo(hasVideo)
}

func (r *LiveKitRecorder) GetHasVideo() bool {
	return r.hasVideo
}

func (r *LiveKitRecorder) SetKeyframeRequester(requester KeyframeRequester) {
	r.webmRecorder.SetKeyframeRequester(requester)
}

func (r *LiveKitRecorder) VideoTimestamp() time.Duration {
	return r.webmRecorder.VideoTimestamp()
}

func (r *LiveKitRecorder) NotifySkippedPacket(seq uint16) {
	r.webmRecorder.NotifySkippedPacket(seq)
}

func (r *LiveKitRecorder) PushVideo(packet *rtp.Packet) {
	log.WithField("session", r.ctx.Value("session")).
		Tracef("Pushing video packet %d", packet.SequenceNumber)
	r.webmRecorder.PushVideo(packet)
}

func (r *LiveKitRecorder) PushAudio(packet *rtp.Packet) {
	log.WithField("session", r.ctx.Value("session")).
		Tracef("Pushing audio packet %d", packet.SequenceNumber)
	r.webmRecorder.PushAudio(packet)
}

// PacketHandler

func (r *LiveKitRecorder) HandleVideoPacket(packet *rtp.Packet) {
	r.PushVideo(packet)
}

func (r *LiveKitRecorder) HandleAudioPacket(packet *rtp.Packet) {
	r.PushAudio(packet)
}
