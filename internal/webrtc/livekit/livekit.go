package livekit

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/interfaces"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/server-sdk-go/v2/pkg/jitter"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	log "github.com/sirupsen/logrus"
)

type MimeType string

const (
	MimeTypeVP8  MimeType = "video/vp8"
	MimeTypeOpus MimeType = "audio/opus"
)

type TrackKind string

const (
	TrackKindVideo TrackKind = "video"
	TrackKindAudio TrackKind = "audio"
)

type PLITracker struct {
	count     int
	timestamp time.Time
}

type LiveKitWebRTC struct {
	m                  sync.Mutex
	ctx                context.Context
	cfg                config.LiveKit
	room               *lksdk.Room
	tracks             map[string]*lksdk.Track
	remoteParticipants map[string]*lksdk.RemoteParticipant
	handler            recorder.Recorder
	roomName           string
	trackIDs           []string
	pliStats           map[uint32]PLITracker
	jitterBuffers      map[string]*jitter.Buffer
	hasAudio           bool
	hasVideo           bool
}

func NewLiveKitWebRTC(ctx context.Context, cfg config.LiveKit, recorder *recorder.WebmRecorder) *LiveKitWebRTC {
	return &LiveKitWebRTC{
		ctx:           ctx,
		cfg:           cfg,
		tracks:        make(map[string]*lksdk.Track),
		handler:       recorder,
		pliStats:      make(map[uint32]PLITracker),
		jitterBuffers: make(map[string]*jitter.Buffer),
	}
}

func (w *LiveKitWebRTC) Connect(room string, trackIDs []string) error {
	sessionID, ok := w.ctx.Value("session").(string)

	if !ok {
		return fmt.Errorf("session ID not found in context")
	}

	identity := fmt.Sprintf("bbb-webrtc-recorder-%s", sessionID)

	log.WithField("session", w.ctx.Value("session")).
		Debugf("Connecting to LiveKit room %s with identity %s", room, identity)

	roomClient, err := lksdk.ConnectToRoom(w.cfg.Host, lksdk.ConnectInfo{
		APIKey:              w.cfg.APIKey,
		APISecret:           w.cfg.APISecret,
		RoomName:            room,
		ParticipantIdentity: identity,
		ParticipantKind:     lksdk.ParticipantEgress,
	}, &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed:   w.onTrackSubscribed,
			OnTrackUnsubscribed: w.onTrackUnsubscribed,
			OnTrackUnmuted:      w.onTrackUnmuted,
			OnTrackMuted:        w.onTrackMuted,
		},
		OnDisconnected: w.onDisconnected,
	},
		lksdk.WithAutoSubscribe(false),
	)

	if err != nil {
		return fmt.Errorf("failed to connect to LiveKit room: %w", err)
	}

	w.room = roomClient
	w.roomName = room
	w.trackIDs = trackIDs

	log.WithField("session", w.ctx.Value("session")).
		Infof("Connected to LiveKit room %s", w.roomName)

	if _, err := w.subscribeToTracks(); err != nil {
		log.WithField("session", w.ctx.Value("session")).
			Errorf("Failed to subscribe to tracks: %v", err)
		return err
	}

	return nil
}

func (w *LiveKitWebRTC) Close() error {
	w.m.Lock()
	defer w.m.Unlock()

	if w.room != nil {
		w.room.Disconnect()
	}

	return nil
}

func (w *LiveKitWebRTC) subscribeToTracks() (subscribedTracks []lksdk.TrackPublication, err error) {
	subscribedTracks = make([]lksdk.TrackPublication, 0)
	remoteParticipants := make(map[string]*lksdk.RemoteParticipant)

	for _, remoteParticipant := range w.room.GetRemoteParticipants() {
		for _, trackPublication := range remoteParticipant.TrackPublications() {
			if slices.Contains(w.trackIDs, trackPublication.SID()) {
				kind := TrackKind(trackPublication.Kind())

				if kind == TrackKindVideo {
					w.hasVideo = true
					w.handler.SetHasVideo(true)
				} else if kind == TrackKindAudio {
					w.hasAudio = true
					w.handler.SetHasAudio(true)
				}

				if err := w.subscribe(trackPublication); err != nil {
					log.WithField("session", w.ctx.Value("session")).
						Errorf("Failed to subscribe to track %s: %v", trackPublication.SID(), err)
					return nil, err
				}

				subscribedTracks = append(subscribedTracks, trackPublication)
				remoteParticipants[remoteParticipant.Identity()] = remoteParticipant
			}
		}
	}

	w.remoteParticipants = remoteParticipants
	return subscribedTracks, nil
}

func (w *LiveKitWebRTC) subscribe(track lksdk.TrackPublication) error {
	if pub, ok := track.(*lksdk.RemoteTrackPublication); ok {
		if pub.IsSubscribed() {
			return nil
		}

		log.WithField("session", w.ctx.Value("session")).
			Debugf("Subscribing to track %s", pub.SID())

		return pub.SetSubscribed(true)
	}

	return fmt.Errorf("unsupported track publication type: %T", track)
}

func (w *LiveKitWebRTC) RequestKeyframe() {
	w.m.Lock()
	defer w.m.Unlock()

	ssrcs := make([]string, 0, len(w.pliStats))

	for ssrc := range w.pliStats {
		ssrcs = append(ssrcs, fmt.Sprintf("%d", ssrc))
	}

	log.WithField("session", w.ctx.Value("session")).
		Tracef("Requesting keyframe for SSRCs %s", strings.Join(ssrcs, ", "))

	for ssrc := range w.pliStats {
		w.RequestKeyframeForSSRC(uint32(ssrc))
	}
}

func (w *LiveKitWebRTC) RequestKeyframeForSSRC(ssrc uint32) {
	if w.room == nil {
		return
	}

	log.WithField("session", w.ctx.Value("session")).
		Tracef("Requesting keyframe for SSRC %d", ssrc)

	// w.remoteParticipants contain all owners of the tracks we are subscribed to
	for _, participant := range w.remoteParticipants {
		participant.WritePLI(webrtc.SSRC(ssrc))

		if _, exists := w.pliStats[ssrc]; !exists {
			w.pliStats[ssrc] = PLITracker{count: 0, timestamp: time.Now()}
		}

		newCount := w.pliStats[ssrc].count + 1
		now := time.Now()
		log.WithField("session", w.ctx.Value("session")).
			Tracef("Sending PLI #%d for SSRC %d to participant %s (sinceLast=%s)",
				newCount, ssrc, participant.Identity(), now.Sub(w.pliStats[ssrc].timestamp))
		w.pliStats[ssrc] = PLITracker{count: newCount, timestamp: now}
	}
}

func (w *LiveKitWebRTC) onTrackSubscribed(
	track *webrtc.TrackRemote,
	pub *lksdk.RemoteTrackPublication,
	rp *lksdk.RemoteParticipant,
) {
	trackKind := TrackKind(pub.Kind())
	trackID := pub.SID()
	isVideo := trackKind == TrackKindVideo
	clockRate := track.Codec().ClockRate
	mimeType := MimeType(strings.ToLower(track.Codec().MimeType))

	log.WithField("session", w.ctx.Value("session")).
		Infof("Subscribed to track %s source=%s kind=%s participant=%s ssrc=%d",
			trackID, pub.Source(), trackKind, rp.Identity(), track.SSRC())

	var depacketizer rtp.Depacketizer
	switch mimeType {
	case MimeTypeVP8:
		depacketizer = &codecs.VP8Packet{}
	case MimeTypeOpus:
		depacketizer = &codecs.OpusPacket{}
	default:
		log.WithField("session", w.ctx.Value("session")).
			Errorf("Unsupported codec: %s", mimeType)
		return
	}

	latency := time.Duration(200) * time.Millisecond
	buffer := jitter.NewBuffer(
		depacketizer,
		clockRate,
		latency,
		jitter.WithPacketDroppedHandler(func() {
			if isVideo {
				w.RequestKeyframe()
			}
		}),
		// TODO: plug logger
	)

	w.m.Lock()
	w.jitterBuffers[trackID] = buffer

	if isVideo {
		ssrc := uint32(track.SSRC())
		w.pliStats[ssrc] = PLITracker{count: 0, timestamp: time.Now()}

		if kfr, ok := w.handler.(interface {
			SetKeyframeRequester(interfaces.KeyframeRequester)
		}); ok {
			kfr.SetKeyframeRequester(w)
		}

		// Start periodic PLI requests - 3s to be similar to the other adapter,
		// but LK is smarter than this. TODO remove later
		done := make(chan bool)
		defer func() {
			done <- true
		}()
		go func() {
			ticker := time.NewTicker(time.Second * 3)
			for {
				select {
				case <-done:
					ticker.Stop()
					return
				case <-ticker.C:
					w.RequestKeyframeForSSRC(ssrc)
				}
			}
		}()
	}
	w.m.Unlock()

	go func() {
		for {
			packet, _, err := track.ReadRTP()
			if err != nil {
				log.WithField("session", w.ctx.Value("session")).
					Errorf("Error reading from track %s: %v", trackID, err)
				return
			}

			buffer.Push(packet)

			packets := buffer.Pop(true)

			for _, p := range packets {
				switch trackKind {
				case TrackKindVideo:
					w.handler.PushVideo(p)
				case TrackKindAudio:
					w.handler.PushAudio(p)
				}
			}
		}
	}()
}

func (w *LiveKitWebRTC) onTrackUnsubscribed(
	track *webrtc.TrackRemote,
	pub *lksdk.RemoteTrackPublication,
	rp *lksdk.RemoteParticipant,
) {
	log.WithField("session", w.ctx.Value("session")).
		Infof("Track %s unsubscribed", pub.SID())
}

func (w *LiveKitWebRTC) onTrackUnmuted(
	pub lksdk.TrackPublication,
	p lksdk.Participant,
) {
	log.WithField("session", w.ctx.Value("session")).
		Infof("Track %s unmuted", pub.SID())
}

func (w *LiveKitWebRTC) onTrackMuted(
	pub lksdk.TrackPublication,
	p lksdk.Participant,
) {
	log.WithField("session", w.ctx.Value("session")).
		Infof("Track %s muted", pub.SID())
}

func (w *LiveKitWebRTC) onDisconnected() {
	log.WithField("session", w.ctx.Value("session")).
		Infof("Disconnected from LiveKit room %s", w.roomName)
}
