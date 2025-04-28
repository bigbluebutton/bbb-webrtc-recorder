package livekit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/interfaces"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/utils"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/server-sdk-go/v2/pkg/jitter"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
)

const (
	MimeTypeVP8  MimeType = "video/vp8"
	MimeTypeOpus MimeType = "audio/opus"
)

const (
	TrackKindVideo TrackKind = "video"
	TrackKindAudio TrackKind = "audio"
)

const (
	notFlowingTicker = time.Millisecond * 100
	flowingTicker    = time.Millisecond * 1000
)

type MimeType string

type TrackKind string

type pliTracker struct {
	count     int
	timestamp time.Time
}

type trackFlowState struct {
	lastSeqNum uint16
	lastRecvTs time.Time
	isFlowing  bool
}

type LiveKitWebRTC struct {
	m                  sync.Mutex
	ctx                context.Context
	cfg                config.LiveKit
	rec                recorder.Recorder
	room               *lksdk.Room
	remoteTrackPubs    map[string]*lksdk.RemoteTrackPublication
	remoteParticipants map[string]*lksdk.RemoteParticipant
	participantIDs     map[string]string // trackID -> participantID
	roomId             string
	trackIds           []string
	pliStats           map[uint32]pliTracker
	jitterBuffers      map[string]*jitter.Buffer
	hasAudio           bool
	hasVideo           bool
	flowState          map[string]*trackFlowState
	flowCallback       func(isFlowing bool, timestamp time.Duration, closed bool)
	trackStats         map[string]*appstats.AdapterTrackStats
	startTs            time.Time
	connStateCallback  func(state utils.ConnectionState)
}

func NewLiveKitWebRTC(
	ctx context.Context,
	cfg config.LiveKit,
	rec recorder.Recorder,
	roomId string,
	trackIds []string,
) *LiveKitWebRTC {
	w := &LiveKitWebRTC{
		ctx:             ctx,
		cfg:             cfg,
		rec:             rec,
		roomId:          roomId,
		trackIds:        trackIds,
		remoteTrackPubs: make(map[string]*lksdk.RemoteTrackPublication),
		pliStats:        make(map[uint32]pliTracker),
		jitterBuffers:   make(map[string]*jitter.Buffer),
		flowState:       make(map[string]*trackFlowState),
		trackStats:      make(map[string]*appstats.AdapterTrackStats),
		participantIDs:  make(map[string]string),
		startTs:         time.Now(),
	}

	w.initTrackStats()

	return w
}

func (w *LiveKitWebRTC) SetConnectionStateCallback(callback func(state utils.ConnectionState)) {
	w.connStateCallback = callback
}

func (w *LiveKitWebRTC) SetFlowCallback(callback func(isFlowing bool, timestamp time.Duration, closed bool)) {
	w.flowCallback = callback
}

func (w *LiveKitWebRTC) Init() error {
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

	room, err := lksdk.ConnectToRoom(w.cfg.Host, lksdk.ConnectInfo{
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
		OnDisconnectedWithReason: w.onDisconnected,
	},
		lksdk.WithAutoSubscribe(false),
	)

	if err != nil {
		return fmt.Errorf("failed to connect to LiveKit room: %w", err)
	}

	w.room = room
	log.WithField("session", w.ctx.Value("session")).
		Infof("Connected to LiveKit room %s", w.roomId)

	if _, err := w.subscribeToTracks(); err != nil {
		log.WithField("session", w.ctx.Value("session")).
			Errorf("Failed to subscribe to tracks: %v", err)
		return err
	}

	return nil
}

func (w *LiveKitWebRTC) Close() time.Duration {
	if w.room != nil {
		w.room.Disconnect()
	}

	if w.rec != nil {
		return w.rec.Close()
	}

	return 0
}

func (w *LiveKitWebRTC) GetStats() *appstats.CaptureStats {
	if len(w.trackIds) == 0 {
		return &appstats.CaptureStats{}
	}

	adapterStats := &appstats.CaptureStats{
		RecorderSessionUUID: w.ctx.Value("session").(string),
		RoomID:              w.roomId,
		// All participants are the same right now for every track here
		ParticipantID: w.participantIDs[w.trackIds[0]],
		FileName:      w.rec.GetFilePath(),
		Tracks:        make(map[string]*appstats.TrackStats),
	}

	// GetStats locks the recorder - don't call it while holding w.m
	recStats := w.rec.GetStats()

	for trackID, remoteTrackPub := range w.remoteTrackPubs {
		trackInfo := remoteTrackPub.TrackInfo()
		mimeType := trackInfo.MimeType

		w.m.Lock()
		var bufferStats *jitter.BufferStats
		jb := w.jitterBuffers[trackID]
		trackStats := w.trackStats[trackID]

		if jb != nil {
			bufferStats = jb.Stats()
		} else {
			bufferStats = &jitter.BufferStats{}
		}

		pliCount := 0

		if remoteTrackPub.Kind() == lksdk.TrackKindVideo {
			for _, tracker := range w.pliStats {
				pliCount += tracker.count
			}
		}

		if trackStats != nil {
			trackStats.PLIRequests = pliCount
		}
		w.m.Unlock()

		source := remoteTrackPub.Source().String()
		adapterStats.Tracks[source] = &appstats.TrackStats{
			Source: source,
			Buffer: &appstats.BufferStatsWrapper{
				PacketsPushed:  bufferStats.PacketsPushed,
				PacketsPopped:  bufferStats.PacketsPopped,
				PacketsDropped: bufferStats.PacketsDropped,
				PaddingPushed:  bufferStats.PaddingPushed,
				SamplesPopped:  bufferStats.SamplesPopped,
			},
			Adapter:   trackStats,
			TrackKind: string(remoteTrackPub.Kind()),
			MimeType:  mimeType,
		}

		// TODO - in the future, propagate track ID data down the chain to get
		// the correct stats for the right track when there are multiple tracks
		// of the same kind
		if remoteTrackPub.Kind() == lksdk.TrackKindVideo {
			adapterStats.Tracks[source].RecorderTrackStats = recStats.Video
		} else if remoteTrackPub.Kind() == lksdk.TrackKindAudio {
			adapterStats.Tracks[source].RecorderTrackStats = recStats.Audio
		}
	}

	return adapterStats
}

func (w *LiveKitWebRTC) RequestKeyframe() {
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
	}

	w.m.Lock()

	if _, exists := w.pliStats[ssrc]; !exists {
		w.pliStats[ssrc] = pliTracker{count: 0, timestamp: time.Now()}
	}

	newCount := w.pliStats[ssrc].count + 1
	now := time.Now()
	w.pliStats[ssrc] = pliTracker{count: newCount, timestamp: now}

	w.m.Unlock()
}

func (w *LiveKitWebRTC) initTrackStats() {
	for _, trackID := range w.trackIds {
		w.trackStats[trackID] = &appstats.AdapterTrackStats{
			StartTime:         time.Now().Unix(),
			FirstSeqNum:       0,
			LastSeqNum:        0,
			SeqNumWrapArounds: 0,
			PLIRequests:       0,
			RTPReadErrors:     0,
		}
	}
}

func (w *LiveKitWebRTC) subscribeToTracks() (subscribedTrackPubs map[string]*lksdk.RemoteTrackPublication, err error) {
	subscribedTrackPubs = make(map[string]*lksdk.RemoteTrackPublication)
	remoteParticipants := make(map[string]*lksdk.RemoteParticipant)

	for _, remoteParticipant := range w.room.GetRemoteParticipants() {
		for _, remoteTrackPublication := range remoteParticipant.TrackPublications() {
			if slices.Contains(w.trackIds, remoteTrackPublication.SID()) {
				kind := TrackKind(remoteTrackPublication.Kind())

				if kind == TrackKindVideo {
					w.hasVideo = true
					w.rec.SetHasVideo(true)
				} else if kind == TrackKindAudio {
					w.hasAudio = true
					w.rec.SetHasAudio(true)
				}

				if remoteTrackPub, ok := remoteTrackPublication.(*lksdk.RemoteTrackPublication); ok {
					if err := w.subscribe(remoteTrackPub); err != nil {
						log.WithField("session", w.ctx.Value("session")).
							Errorf("Failed to subscribe to track %s: %v", remoteTrackPublication.SID(), err)
						return nil, err
					}

					subscribedTrackPubs[remoteTrackPublication.SID()] = remoteTrackPub
					w.participantIDs[remoteTrackPublication.SID()] = remoteParticipant.Identity()
					remoteParticipants[remoteParticipant.Identity()] = remoteParticipant
				}
			}
		}
	}

	w.remoteParticipants = remoteParticipants
	w.remoteTrackPubs = subscribedTrackPubs

	return subscribedTrackPubs, nil
}

func (w *LiveKitWebRTC) subscribe(track lksdk.TrackPublication) error {
	if pub, ok := track.(*lksdk.RemoteTrackPublication); ok {
		if pub.IsSubscribed() {
			return nil
		}

		log.WithField("session", w.ctx.Value("session")).
			Debugf("Subscribing to track %s", pub.SID())

		if pub.Kind() == lksdk.TrackKindVideo {
			log.WithField("session", w.ctx.Value("session")).
				Debugf("Setting video quality to %s for track %s", w.cfg.PreferredVideoQuality, pub.SID())
			// Ignore error - only throws when video quality = OFF which we do not care about
			_ = pub.SetVideoQuality(w.cfg.PreferredVideoQuality)
		}

		return pub.SetSubscribed(true)
	}

	return fmt.Errorf("unsupported track publication type: %T", track)
}

func (w *LiveKitWebRTC) updateFlowState(trackID string, seqNum uint16, recvTs time.Time) {
	w.m.Lock()
	defer w.m.Unlock()

	state, exists := w.flowState[trackID]

	if !exists {
		state = &trackFlowState{
			lastSeqNum: seqNum,
			lastRecvTs: recvTs,
			isFlowing:  false,
		}
		w.flowState[trackID] = state

		return
	}

	wasFlowing := state.isFlowing
	state.isFlowing = state.lastSeqNum != seqNum
	state.lastSeqNum = seqNum
	state.lastRecvTs = recvTs

	if state.isFlowing != wasFlowing && w.flowCallback != nil {
		var latestRecvTs time.Time

		for _, s := range w.flowState {
			if s.lastRecvTs.After(latestRecvTs) {
				latestRecvTs = s.lastRecvTs
			}
		}

		w.flowCallback(state.isFlowing, latestRecvTs.Sub(w.startTs), false)
		log.WithField("session", w.ctx.Value("session")).
			Tracef("Flow state changed for track %s: flowing=%v latestRecvTs=%s",
				trackID, state.isFlowing, latestRecvTs)
	}
}

func (w *LiveKitWebRTC) onTrackSubscribed(
	track *webrtc.TrackRemote,
	pub *lksdk.RemoteTrackPublication,
	rp *lksdk.RemoteParticipant,
) {
	trackID := pub.SID()
	trackKind := TrackKind(pub.Kind())
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

	appstats.TrackRecordingStarted(string(trackKind), string(mimeType), pub.Source().String())
	w.jitterBuffers[trackID] = buffer

	if isVideo {
		ssrc := uint32(track.SSRC())

		w.m.Lock()
		if _, exists := w.pliStats[ssrc]; !exists {
			w.pliStats[ssrc] = pliTracker{count: 0, timestamp: time.Now()}
		}
		w.m.Unlock()

		if kfr, ok := w.rec.(interface {
			SetKeyframeRequester(interfaces.KeyframeRequester)
		}); ok {
			kfr.SetKeyframeRequester(w)
		}
	}

	flowCheckDone := make(chan bool)
	defer func() {
		flowCheckDone <- true
	}()
	go func() {
		ticker := time.NewTicker(notFlowingTicker)
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
					w.updateFlowState(trackID, state.lastSeqNum, state.lastRecvTs)
				}

				if state.isFlowing {
					ticker.Reset(flowingTicker)
				} else {
					ticker.Reset(notFlowingTicker)
				}

				lastSeqNum = state.lastSeqNum
				w.m.Unlock()
			}
		}
	}()

	go func() {
		for {
			readDeadline := time.Now().Add(w.cfg.PacketReadTimeout)
			// Ignore error from SetReadDeadline - it comes from pion/packetio
			// but it'll never throw - probably conforming to some interface
			_ = track.SetReadDeadline(readDeadline)
			packet, _, err := track.ReadRTP()

			if err != nil {
				if procErr := w.handleReadRTPError(err, trackID, pub); procErr != nil {
					if procErr == io.EOF {
						log.WithField("session", w.ctx.Value("session")).
							Infof("%s track=%s stopped", pub.MimeType(), trackID)
					} else {
						log.WithField("session", w.ctx.Value("session")).
							Errorf("Unexpected error handling RTP packet from track %s: %v", trackID, procErr)
						// TODO this should be a panic-like situation
					}

					return
				}
			}

			if packet == nil {
				continue
			}

			buffer.Push(packet)
			packets := buffer.Pop(false)

			if len(packets) == 0 {
				continue
			}

			recvTs := time.Now()

			for _, p := range packets {
				switch trackKind {
				case TrackKindVideo:
					w.rec.PushVideo(p)
				case TrackKindAudio:
					w.rec.PushAudio(p)
				}
			}

			w.updateFlowState(trackID, packets[0].SequenceNumber, recvTs)
			w.processPacketStats(trackID, packets)
		}
	}()
}

func (w *LiveKitWebRTC) trackReadErrorStats(err error, trackID string, pub *lksdk.RemoteTrackPublication) {
	if err != io.EOF {
		appstats.OnRTPReadError(pub.Source().String(), string(pub.Kind()), string(pub.MimeType()), err.Error())
		w.m.Lock()
		w.trackStats[trackID].RTPReadErrors++
		w.m.Unlock()
	}
}

func (w *LiveKitWebRTC) handleReadRTPError(err error, trackID string, pub *lksdk.RemoteTrackPublication) error {
	var netErr net.Error
	flowState := w.flowState[trackID]

	log.WithField("session", w.ctx.Value("session")).
		Tracef("Error reading RTP packet from track %s: %+v", trackID, err)
	w.trackReadErrorStats(err, trackID, pub)

	switch {
	case errors.As(err, &netErr) && netErr.Timeout():
		// If the flow state is nil, it means the track is not being tracked yet,
		// so it's not flowing - skip
		if flowState == nil || !flowState.isFlowing {
			return nil
		}

		log.WithField("session", w.ctx.Value("session")).
			Warnf("Network error reading RTP packet from track %s: %v", trackID, err)
		// Update the flow state to indicate the track is not flowing. Nothing much
		// else we can do here.
		w.updateFlowState(trackID, flowState.lastSeqNum, flowState.lastRecvTs)

	case err.Error() == "buffer too small":
		log.WithField("session", w.ctx.Value("session")).
			Warnf("Buffer too small reading RTP packet from track %s", trackID)

	case err.Error() == "EOF" || err == io.EOF:
		log.WithField("session", w.ctx.Value("session")).
			Infof("%s track stopped", pub.MimeType())
		return err

	default:
		log.WithField("session", w.ctx.Value("session")).
			Errorf("Unexpected error handling RTP packet from track %s: %v", trackID, err)

		return err
	}

	return nil
}

func (w *LiveKitWebRTC) processPacketStats(trackID string, packets []*rtp.Packet) {
	w.m.Lock()
	defer w.m.Unlock()

	firstPacket := packets[0]
	lastPacket := packets[len(packets)-1]
	stats := w.trackStats[trackID]

	// This method receives packets from unforced jitter buffer packet pops, which means they're
	// properly ordered. Check is simpler this way. TODO review gaps larger than 2^16/2
	if lastPacket.SequenceNumber < firstPacket.SequenceNumber ||
		firstPacket.SequenceNumber < stats.LastSeqNum {
		stats.SeqNumWrapArounds++
	}

	stats.LastSeqNum = lastPacket.SequenceNumber

	log.WithField("session", w.ctx.Value("session")).
		Tracef("Processed packet batch for track %s: lastSeqNum: %d, firstSeqNum: %d, wraparound: %d, firstPacket: %d, lastPacket: %d",
			trackID, stats.LastSeqNum, stats.FirstSeqNum, stats.SeqNumWrapArounds, firstPacket.SequenceNumber, lastPacket.SequenceNumber)
}

func (w *LiveKitWebRTC) onTrackUnsubscribed(
	track *webrtc.TrackRemote,
	pub *lksdk.RemoteTrackPublication,
	rp *lksdk.RemoteParticipant,
) {
	trackID := pub.SID()
	trackKind := TrackKind(pub.Kind())
	mimeType := MimeType(strings.ToLower(track.Codec().MimeType))

	w.m.Lock()

	if stats, ok := w.trackStats[trackID]; ok {
		stats.EndTime = time.Now().Unix()
	}

	w.m.Unlock()

	log.WithField("session", w.ctx.Value("session")).
		Infof("Unsubscribed from track %s source=%s kind=%s participant=%s ssrc=%d",
			trackID, pub.Source(), trackKind, rp.Identity(), track.SSRC())

	appstats.TrackRecordingStopped(string(trackKind), string(mimeType), pub.Source().String())
}

func (w *LiveKitWebRTC) onTrackUnmuted(
	pub lksdk.TrackPublication,
	p lksdk.Participant,
) {
	trackID := pub.SID()

	if _, exists := w.remoteTrackPubs[trackID]; !exists {
		return
	}

	log.WithField("session", w.ctx.Value("session")).
		Infof("Track unmuted: %s", trackID)
}

func (w *LiveKitWebRTC) onTrackMuted(
	pub lksdk.TrackPublication,
	p lksdk.Participant,
) {
	trackID := pub.SID()

	if _, exists := w.remoteTrackPubs[trackID]; !exists {
		return
	}

	log.WithField("session", w.ctx.Value("session")).
		Infof("Track muted: %s", trackID)
}

func (w *LiveKitWebRTC) onDisconnected(reason lksdk.DisconnectionReason) {
	log.WithField("session", w.ctx.Value("session")).
		Infof("Disconnected from LiveKit room=%s reason=%v", w.roomId, reason)

	state := utils.NormalizeLiveKitDisconnectReason(reason)

	if w.connStateCallback != nil {
		w.connStateCallback(state)
	}

	// If this is a terminal state, also notify via flow callback
	if state.IsTerminalState() && w.flowCallback != nil {
		w.flowCallback(false, time.Since(w.startTs), true)
	}
}

func (w *LiveKitWebRTC) validateInitParams() error {
	if w.flowCallback == nil {
		return fmt.Errorf("flowCallback is not set")
	}

	if w.rec == nil {
		return fmt.Errorf("recorder is not set")
	}

	return nil
}
