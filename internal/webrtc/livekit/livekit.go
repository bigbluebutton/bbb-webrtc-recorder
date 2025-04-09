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

type TrackFlowState struct {
	lastSeqNum    uint16
	lastTimestamp time.Duration
	isFlowing     bool
}

type LiveKitWebRTC struct {
	m                  sync.Mutex
	ctx                context.Context
	cfg                config.LiveKit
	room               *lksdk.Room
	tracks             map[string]*lksdk.Track
	remoteParticipants map[string]*lksdk.RemoteParticipant
	handler            recorder.Recorder
	roomId             string
	trackIds           []string
	pliStats           map[uint32]PLITracker
	jitterBuffers      map[string]*jitter.Buffer
	hasAudio           bool
	hasVideo           bool
	flowState          map[string]*TrackFlowState
	flowCallback       func(bool, time.Duration, bool)
}

func NewLiveKitWebRTC(
	ctx context.Context,
	cfg config.LiveKit,
	roomId string,
	trackIds []string,
) *LiveKitWebRTC {
	return &LiveKitWebRTC{
		ctx:           ctx,
		cfg:           cfg,
		roomId:        roomId,
		trackIds:      trackIds,
		tracks:        make(map[string]*lksdk.Track),
		pliStats:      make(map[uint32]PLITracker),
		jitterBuffers: make(map[string]*jitter.Buffer),
		flowState:     make(map[string]*TrackFlowState),
	}
}

func (w *LiveKitWebRTC) SetFlowCallback(callback func(isFlowing bool, timestamp time.Duration, closed bool)) {
	w.flowCallback = callback
}

func (w *LiveKitWebRTC) Init(rec recorder.Recorder) error {
	w.handler = rec

	if err := w.validateInitParams(); err != nil {
		return err
	}

	sessionID, ok := w.ctx.Value("session").(string)

	if !ok {
		return fmt.Errorf("session ID not found in context")
	}

	identity := fmt.Sprintf("bbb-webrtc-recorder-%s", sessionID)

	log.WithField("session", w.ctx.Value("session")).
		Debugf("Connecting to LiveKit room %s with identity %s", w.roomId, identity)

	roomClient, err := lksdk.ConnectToRoom(w.cfg.Host, lksdk.ConnectInfo{
		APIKey:              w.cfg.APIKey,
		APISecret:           w.cfg.APISecret,
		RoomName:            w.roomId,
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

	log.WithField("session", w.ctx.Value("session")).
		Infof("Connected to LiveKit room %s", w.roomId)

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
			if slices.Contains(w.trackIds, trackPublication.SID()) {
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

func (w *LiveKitWebRTC) updateFlowState(trackID string, seqNum uint16, timestamp time.Duration) {
	w.m.Lock()
	defer w.m.Unlock()

	state, exists := w.flowState[trackID]

	if !exists {
		state = &TrackFlowState{
			lastSeqNum:    seqNum,
			lastTimestamp: timestamp,
			isFlowing:     false,
		}
		w.flowState[trackID] = state

		return
	}

	wasFlowing := state.isFlowing
	state.isFlowing = state.lastSeqNum != seqNum
	state.lastSeqNum = seqNum
	state.lastTimestamp = timestamp

	if state.isFlowing != wasFlowing && w.flowCallback != nil {
		var latestTimestamp time.Duration

		for _, s := range w.flowState {
			if s.lastTimestamp > latestTimestamp {
				latestTimestamp = s.lastTimestamp
			}
		}

		w.flowCallback(state.isFlowing, latestTimestamp, false)
		log.WithField("session", w.ctx.Value("session")).
			Tracef("Flow state changed for track %s: flowing=%v latestTimestamp=%s",
				trackID, state.isFlowing, latestTimestamp)
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

	flowCheckDone := make(chan bool)
	defer func() {
		flowCheckDone <- true
	}()
	go func() {
		ticker := time.NewTicker(time.Millisecond * 100)
		var lastSeqNum uint16
		for {
			select {
			case <-flowCheckDone:
				ticker.Stop()
				return
			case <-ticker.C:
				w.m.Lock()
				state, exists := w.flowState[trackID]

				if exists && state.lastSeqNum == lastSeqNum {
					// No new packets received == not flowing
					w.updateFlowState(trackID, state.lastSeqNum, state.lastTimestamp)
				}

				if state.isFlowing {
					ticker.Reset(time.Millisecond * 1000)
				} else {
					ticker.Reset(time.Millisecond * 100)
				}

				lastSeqNum = state.lastSeqNum
				w.m.Unlock()
			}
		}
	}()

	go func() {
		for {
			packet, _, err := track.ReadRTP()

			if err != nil {
				log.WithField("session", w.ctx.Value("session")).
					Errorf("Error reading from track %s: %v", trackID, err)
				return
			}

			buffer.Push(packet)
			packets := buffer.Pop(false)

			if len(packets) == 0 {
				continue
			}

			for _, p := range packets {
				w.updateFlowState(
					trackID,
					p.SequenceNumber,
					time.Duration(p.Timestamp)*time.Millisecond/time.Duration(clockRate),
				)

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
		Infof("Disconnected from LiveKit room %s", w.roomId)
}

func (w *LiveKitWebRTC) validateInitParams() error {
	if w.flowCallback == nil {
		return fmt.Errorf("flowCallback is not set")
	}

	if w.handler == nil {
		return fmt.Errorf("handler is not set")
	}

	return nil
}
