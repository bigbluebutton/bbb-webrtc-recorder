package livekit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/appstats"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/types"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/interfaces"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/utils"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
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
	notFlowingTicker   = time.Millisecond * 100
	flowingTicker      = time.Millisecond * 1000
	tokenTTL           = 24 * time.Hour
	baseSystemMetadata = "{\"bbb_system\": true}"
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
	identity           string
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
	rtpWriters         map[string]*recorder.RTPWriter

	keyframeRequestChan   chan uint32
	requestKeyframeCtx    context.Context
	requestKeyframeCancel context.CancelFunc
	requestKeyframeWg     sync.WaitGroup

	pendingSubscriptions map[string]time.Time
}

func NewLiveKitWebRTC(
	ctx context.Context,
	cfg config.LiveKit,
	rec recorder.Recorder,
	roomId string,
	trackIds []string,
) *LiveKitWebRTC {
	requestKeyframeCtx, requestKeyframeCancel := context.WithCancel(ctx)
	sessionID := ctx.Value("session").(string)
	identity := fmt.Sprintf("bbb-webrtc-recorder-%s", sessionID)

	w := &LiveKitWebRTC{
		ctx:                   ctx,
		cfg:                   cfg,
		rec:                   rec,
		roomId:                roomId,
		identity:              identity,
		trackIds:              trackIds,
		remoteTrackPubs:       make(map[string]*lksdk.RemoteTrackPublication),
		pliStats:              make(map[uint32]pliTracker),
		jitterBuffers:         make(map[string]*jitter.Buffer),
		flowState:             make(map[string]*trackFlowState),
		trackStats:            make(map[string]*appstats.AdapterTrackStats),
		participantIDs:        make(map[string]string),
		rtpWriters:            make(map[string]*recorder.RTPWriter),
		startTs:               time.Now(),
		keyframeRequestChan:   make(chan uint32, 100),
		requestKeyframeCtx:    requestKeyframeCtx,
		requestKeyframeCancel: requestKeyframeCancel,
		pendingSubscriptions:  make(map[string]time.Time),
	}

	w.initTrackStats()

	w.requestKeyframeWg.Add(1)
	go w.processKeyframeRequests()

	return w
}

func (w *LiveKitWebRTC) SetConnectionStateCallback(callback func(state utils.ConnectionState)) {
	// Lock the mutex before accessing the callback as it's accessed from multiple goroutines
	w.m.Lock()
	defer w.m.Unlock()

	w.connStateCallback = callback
}

func (w *LiveKitWebRTC) SetFlowCallback(callback func(isFlowing bool, timestamp time.Duration, closed bool)) {
	// Lock the mutex before accessing the callback as it's accessed from multiple goroutines
	w.m.Lock()
	defer w.m.Unlock()

	w.flowCallback = callback
}

func (w *LiveKitWebRTC) Init() error {
	if err := w.validateInitParams(); err != nil {
		return err
	}

	if err := w.connectToRoom(); err != nil {
		return err
	}

	if _, err := w.subscribeToTracks(w.trackIds); err != nil {
		w.Close()

		log.WithField("session", w.ctx.Value("session")).
			WithField("room", w.roomId).
			WithField("identity", w.identity).
			WithField("trackIds", w.trackIds).
			Errorf("Failed to subscribe to tracks: %v", err)
		appstats.OnTrackSubscriptionFailed(err.Error())

		return err
	}

	return nil
}

func (w *LiveKitWebRTC) Close() time.Duration {
	if w.room != nil {
		w.room.Disconnect()
	}

	if w.requestKeyframeCancel != nil {
		w.requestKeyframeCancel()
	}

	w.requestKeyframeWg.Wait()

	if w.rtpWriters != nil {
		for trackID, writer := range w.rtpWriters {
			if err := writer.Close(); err != nil {
				log.WithField("session", w.ctx.Value("session")).
					WithField("trackID", trackID).
					Warnf("Failed to close rtp writer for track %s: %v", trackID, err)
			}
		}
	}

	w.rtpWriters = nil

	if w.rec != nil {
		return w.rec.Close()
	}

	return 0
}

func (w *LiveKitWebRTC) GetStats() *appstats.CaptureStats {
	w.m.Lock()

	if len(w.trackIds) == 0 {
		w.m.Unlock()
		return &appstats.CaptureStats{}
	}

	var currentParticipantID string

	if len(w.trackIds) > 0 {
		// All participants have the same participantID for the tracks we are subscribed to
		if pid, ok := w.participantIDs[w.trackIds[0]]; ok {
			currentParticipantID = pid
		}
	}

	remoteTrackPubs := make(map[string]*lksdk.RemoteTrackPublication, len(w.remoteTrackPubs))
	for k, v := range w.remoteTrackPubs {
		remoteTrackPubs[k] = v
	}

	jitterBuffers := make(map[string]*jitter.Buffer, len(w.jitterBuffers))
	for k, v := range w.jitterBuffers {
		jitterBuffers[k] = v
	}

	pliStats := make(map[uint32]pliTracker, len(w.pliStats))
	for k, v := range w.pliStats {
		pliStats[k] = v
	}

	trackStats := make(map[string]appstats.AdapterTrackStats, len(w.trackStats))
	for k, vPtr := range w.trackStats {
		if vPtr != nil {
			trackStats[k] = *vPtr
		}
	}

	var recorderFilePath string
	var recorderRef recorder.Recorder
	if w.rec != nil {
		recorderFilePath = w.rec.GetFilePath()
		recorderRef = w.rec
	}

	w.m.Unlock()

	var recStats *types.RecorderStats

	if recorderRef != nil {
		recStats = recorderRef.GetStats()
	} else {
		recStats = &types.RecorderStats{}
	}

	finalAdapterStats := &appstats.CaptureStats{
		RecorderSessionUUID: w.ctx.Value("session").(string),
		RoomID:              w.roomId,
		ParticipantID:       currentParticipantID,
		FileName:            recorderFilePath,
		Tracks:              make(map[string]*appstats.TrackStats),
	}

	for trackID, remoteTrackPub := range w.remoteTrackPubs {
		trackInfo := remoteTrackPub.TrackInfo()
		mimeType := trackInfo.MimeType

		var bufferStatsData *jitter.BufferStats

		if jb, ok := w.jitterBuffers[trackID]; ok && jb != nil {
			bufferStatsData = jb.Stats()
		} else {
			bufferStatsData = &jitter.BufferStats{}
		}

		currentTrackAdapterStats := appstats.AdapterTrackStats{}

		if ts, ok := w.trackStats[trackID]; ok {
			currentTrackAdapterStats = *ts
		}

		pliCount := 0
		if remoteTrackPub.Kind() == lksdk.TrackKindVideo {
			for _, tracker := range w.pliStats {
				pliCount += tracker.count
			}
		}
		currentTrackAdapterStats.PLIRequests = pliCount

		source := remoteTrackPub.Source().String()
		finalAdapterStats.Tracks[source] = &appstats.TrackStats{
			Source: source,
			Buffer: &appstats.BufferStatsWrapper{
				PacketsPushed:  bufferStatsData.PacketsPushed,
				PacketsPopped:  bufferStatsData.PacketsPopped,
				PacketsDropped: bufferStatsData.PacketsDropped,
				PaddingPushed:  bufferStatsData.PaddingPushed,
				SamplesPopped:  bufferStatsData.SamplesPopped,
			},
			Adapter:   &currentTrackAdapterStats,
			TrackKind: string(remoteTrackPub.Kind()),
			MimeType:  mimeType,
		}

		if recStats != nil {
			if remoteTrackPub.Kind() == lksdk.TrackKindVideo && recStats.Video != nil {
				finalAdapterStats.Tracks[source].RecorderTrackStats = recStats.Video
			} else if remoteTrackPub.Kind() == lksdk.TrackKindAudio && recStats.Audio != nil {
				finalAdapterStats.Tracks[source].RecorderTrackStats = recStats.Audio
			}
		}
	}
	return finalAdapterStats
}

func (w *LiveKitWebRTC) queueKeyframeRequest(ssrc uint32, reason string) {
	if w.keyframeRequestChan == nil {
		return
	}

	select {
	case w.keyframeRequestChan <- ssrc:
		log.WithFields(log.Fields{
			"session": w.ctx.Value("session"),
			"ssrc":    ssrc,
			"reason":  reason,
		}).Trace("Queued keyframe request")
	case <-w.requestKeyframeCtx.Done():
		return
	default:
		return
	}
}

func (w *LiveKitWebRTC) RequestKeyframe() {
	w.m.Lock()
	ssrcsToRequest := make([]uint32, 0, len(w.pliStats))
	for ssrc := range w.pliStats {
		ssrcsToRequest = append(ssrcsToRequest, ssrc)
	}
	w.m.Unlock()

	if len(ssrcsToRequest) > 0 {
		log.WithField("session", w.ctx.Value("session")).
			Tracef("RequestKeyframe: Queuing PLI for SSRCs %v", ssrcsToRequest)
	}

	for _, ssrc := range ssrcsToRequest {
		w.queueKeyframeRequest(ssrc, "direct_request")
	}
}

func (w *LiveKitWebRTC) RequestKeyframeForSSRC(ssrc uint32) {
	if w.room == nil {
		return
	}

	log.WithField("session", w.ctx.Value("session")).
		Tracef("Requesting keyframe for SSRC %d", ssrc)
	requestedKeyframes := 0

	// w.remoteParticipants contain all owners of the tracks we are subscribed to
	for _, participant := range w.remoteParticipants {
		participant.WritePLI(webrtc.SSRC(ssrc))
		requestedKeyframes++
	}

	if requestedKeyframes > 0 {
		log.WithField("session", w.ctx.Value("session")).
			Debugf("Requested %d keyframes for SSRC %d", requestedKeyframes, ssrc)
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

func (w *LiveKitWebRTC) HasTrack(trackID string) bool {
	w.m.Lock()
	defer w.m.Unlock()

	return slices.Contains(w.trackIds, trackID)
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

func (w *LiveKitWebRTC) subscribeToTracks(trackIds []string) (subscribedTrackPubs map[string]*lksdk.RemoteTrackPublication, err error) {
	subscribedTrackPubs = make(map[string]*lksdk.RemoteTrackPublication)
	remoteParticipants := make(map[string]*lksdk.RemoteParticipant)
	pendingSubscriptions := make(map[string]time.Time)

	for _, remoteParticipant := range w.room.GetRemoteParticipants() {
		for _, remoteTrackPublication := range remoteParticipant.TrackPublications() {
			if slices.Contains(trackIds, remoteTrackPublication.SID()) {
				kind := TrackKind(remoteTrackPublication.Kind())

				if kind == TrackKindVideo {
					w.hasVideo = true
					w.rec.SetHasVideo(true)
				} else if kind == TrackKindAudio {
					w.hasAudio = true
					w.rec.SetHasAudio(true)
				}

				if remoteTrackPub, ok := remoteTrackPublication.(*lksdk.RemoteTrackPublication); ok {
					trackSID := remoteTrackPub.SID()
					subscribedTrackPubs[trackSID] = remoteTrackPub
					w.participantIDs[trackSID] = remoteParticipant.Identity()
					remoteParticipants[remoteParticipant.Identity()] = remoteParticipant
					pendingSubscriptions[trackSID] = time.Now()

					if err := w.subscribe(remoteTrackPub); err != nil {
						log.WithField("session", w.ctx.Value("session")).
							Errorf("Failed to subscribe to track %s: %v", trackSID, err)
						return nil, err
					}
				}
			}
		}
	}

	w.remoteParticipants = remoteParticipants
	w.remoteTrackPubs = subscribedTrackPubs

	if len(pendingSubscriptions) == 0 {
		return nil, fmt.Errorf("no tracks available")
	}

	w.m.Lock()
	w.pendingSubscriptions = pendingSubscriptions
	w.m.Unlock()

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

func (w *LiveKitWebRTC) updateFlowState(trackID string, seqNum uint16, recvTs time.Time) bool {
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

		// Only deems something as flowing after at least two packets have been received
		return false
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

	return state.isFlowing
}

func (w *LiveKitWebRTC) onTrackSubscribed(
	track *webrtc.TrackRemote,
	pub *lksdk.RemoteTrackPublication,
	rp *lksdk.RemoteParticipant,
) {
	w.observeSubscription(pub.SID())
	trackID := pub.SID()
	trackKind := TrackKind(pub.Kind())
	isVideo := trackKind == TrackKindVideo
	clockRate := track.Codec().ClockRate
	mimeType := MimeType(strings.ToLower(track.Codec().MimeType))

	log.WithField("session", w.ctx.Value("session")).
		Infof("Subscribed to track %s source=%s kind=%s mime=%s clockRate=%d participant=%s ssrc=%d",
			trackID, pub.Source(), trackKind, mimeType, clockRate, rp.Identity(), track.SSRC())

	var depacketizer rtp.Depacketizer
	switch mimeType {
	case MimeTypeVP8:
		depacketizer = &codecs.VP8Packet{}
	case MimeTypeOpus:
		depacketizer = &codecs.OpusPacket{}
	default:
		log.WithField("session", w.ctx.Value("session")).
			Errorf("Unsupported codec: %s", mimeType)
		w.connStateCallback(utils.ConnectionStateFailed)

		return
	}

	if w.cfg.WriteRTPDump {
		basePath := w.rec.GetFilePath()
		ext := filepath.Ext(basePath)
		rtpPath := fmt.Sprintf("%s.rtp", basePath[:len(basePath)-len(ext)])
		rtpWriter, err := recorder.NewRTPWriter(rtpPath, net.IP{0, 0, 0, 0}, 0)

		if err != nil {
			log.WithField("session", w.ctx.Value("session")).
				WithField("trackID", trackID).
				Errorf("Failed to create RTP writer for track %s: %v", trackID, err)
		} else {
			log.WithField("session", w.ctx.Value("session")).
				WithField("trackID", trackID).
				Infof("RTP dump enabled for track %s to %s", trackID, rtpPath)
			w.rtpWriters[trackID] = rtpWriter
		}
	}

	w.m.Lock()
	w.remoteTrackPubs[trackID] = pub
	w.participantIDs[trackID] = rp.Identity()

	// Ensure remoteParticipants map is initialized - might be a thing if autoSubscribe is true
	if w.remoteParticipants == nil {
		w.remoteParticipants = make(map[string]*lksdk.RemoteParticipant)
	}

	w.remoteParticipants[rp.Identity()] = rp
	w.m.Unlock()

	latency := time.Duration(200) * time.Millisecond

	var ssrcForHandler uint32

	if track != nil {
		ssrcForHandler = uint32(track.SSRC())
	}

	buffer := jitter.NewBuffer(
		depacketizer,
		clockRate,
		latency,
		jitter.WithPacketDroppedHandler(func() {
			if isVideo && track != nil {
				w.queueKeyframeRequest(ssrcForHandler, "packet_drop")
			}
		}),
		// TODO: plug logger via jitter.WithLogger(logger)
	)

	w.jitterBuffers[trackID] = buffer

	if isVideo {
		w.m.Lock()
		if _, exists := w.pliStats[ssrcForHandler]; !exists {
			w.pliStats[ssrcForHandler] = pliTracker{count: 0, timestamp: time.Now()}
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
				var currentSeqNum uint16
				var currentRecvTs time.Time
				var isFlowing bool
				var staleSeqNum bool

				w.m.Lock()
				state, exists := w.flowState[trackID]

				if exists {
					currentSeqNum = state.lastSeqNum
					currentRecvTs = state.lastRecvTs
					isFlowing = state.isFlowing
					staleSeqNum = currentSeqNum == lastSeqNum
				}
				w.m.Unlock()

				if staleSeqNum {
					// No new packets received == not flowing
					isFlowing = w.updateFlowState(trackID, currentSeqNum, currentRecvTs)
				}

				if isFlowing {
					ticker.Reset(flowingTicker)
				} else {
					ticker.Reset(notFlowingTicker)
				}

				lastSeqNum = currentSeqNum
			}
		}
	}()

	appstats.OnTrackRecordingStarted(string(trackKind), string(mimeType), pub.Source().String())

	go func() {
		defer func() {
			appstats.OnTrackRecordingStopped(string(trackKind), string(mimeType), pub.Source().String())
			log.WithField("session", w.ctx.Value("session")).
				WithField("trackID", trackID).
				WithField("source", pub.Source().String()).
				WithField("kind", trackKind).
				WithField("mime", mimeType).
				WithField("ssrc", track.SSRC()).
				Info("Track recording stopped")

			// If a panic occurs, notify the connection state callback as failed so clients can retry/handle it
			if err := recover(); err != nil {
				log.WithField("session", w.ctx.Value("session")).
					WithField("error", err).
					WithField("stack", string(debug.Stack())).
					Error("Panic detected in LiveKit packet processing, emit failed state")

				w.connStateCallback(utils.ConnectionStateFailed)
			} else {
				w.connStateCallback(utils.ConnectionStateClosed)
			}
		}()

		rtpWriter, rtpWriterExists := w.rtpWriters[trackID]

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

			if rtpWriterExists {
				if err := rtpWriter.WriteRTP(packet); err != nil {
					log.WithField("session", w.ctx.Value("session")).
						WithField("trackID", trackID).
						Warnf("failed to write RTP packet for track %s: %v", trackID, err)
				}
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

func (w *LiveKitWebRTC) observeSubscription(trackID string) {
	w.m.Lock()
	defer w.m.Unlock()

	if startTime, ok := w.pendingSubscriptions[trackID]; ok && !startTime.IsZero() {
		duration := time.Since(startTime)
		appstats.ObserveLiveKitSubscribeDuration(duration)
		delete(w.pendingSubscriptions, trackID)
	}
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
	if !w.HasTrack(pub.SID()) {
		return
	}

	trackID := pub.SID()
	trackKind := TrackKind(pub.Kind())

	w.m.Lock()

	if stats, ok := w.trackStats[trackID]; ok {
		stats.EndTime = time.Now().Unix()
	}

	w.m.Unlock()

	log.WithField("session", w.ctx.Value("session")).
		Infof("Unsubscribed from track %s source=%s kind=%s participant=%s ssrc=%d",
			trackID, pub.Source(), trackKind, rp.Identity(), track.SSRC())
}

func (w *LiveKitWebRTC) onTrackSubscriptionFailed(
	trackID string,
	rp *lksdk.RemoteParticipant,
) {
	if !w.HasTrack(trackID) {
		return
	}

	w.connStateCallback(utils.ConnectionStateFailed)

	log.WithField("session", w.ctx.Value("session")).
		WithField("room", w.roomId).
		WithField("identity", w.identity).
		WithField("trackID", trackID).
		Errorf("Track subscription failed")
	appstats.OnTrackSubscriptionFailed("livekit_failure")
}

func (w *LiveKitWebRTC) onTrackUnpublished(
	pub *lksdk.RemoteTrackPublication,
	rp *lksdk.RemoteParticipant,
) {
	trackID := pub.SID()

	if !w.HasTrack(trackID) {
		return
	}

	log.WithField("session", w.ctx.Value("session")).
		WithField("room", w.roomId).
		WithField("identity", w.identity).
		WithField("trackID", trackID).
		Infof("Track unpublished")
}

func (w *LiveKitWebRTC) onTrackUnmuted(
	pub lksdk.TrackPublication,
	p lksdk.Participant,
) {
	trackID := pub.SID()

	// Only track mute/unmute events for tracks we're *subscribed* to
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

	// Only track mute/unmute events for tracks we're *subscribed* to
	if _, exists := w.remoteTrackPubs[trackID]; !exists {
		return
	}

	log.WithField("session", w.ctx.Value("session")).
		Infof("Track muted: %s", trackID)
}

func (w *LiveKitWebRTC) onDisconnected(reason lksdk.DisconnectionReason) {
	log.WithField("session", w.ctx.Value("session")).
		WithField("room", w.roomId).
		WithField("identity", w.identity).
		Infof("Disconnected from LiveKit room reason=%v", reason)

	state := utils.NormalizeLiveKitDisconnectReason(reason)

	// Lock the mutex before accessing the callback as it's accessed from multiple goroutines
	w.m.Lock()
	callback := w.connStateCallback
	w.m.Unlock()

	if callback != nil {
		callback(state)
	}

	// If this is a terminal state, also notify via flow callback
	// Lock the mutex before accessing the callback as it's accessed from multiple goroutines
	w.m.Lock()
	fcb := w.flowCallback
	w.m.Unlock()

	if state.IsTerminalState() && fcb != nil {
		fcb(false, time.Since(w.startTs), true)
	}
}

func (w *LiveKitWebRTC) onReconnecting() {
	log.WithField("session", w.ctx.Value("session")).
		WithField("room", w.roomId).
		WithField("identity", w.identity).
		Warn("Reconnecting to LiveKit room")
	appstats.OnParticipantReconnecting()
}

func (w *LiveKitWebRTC) onReconnected() {
	log.WithField("session", w.ctx.Value("session")).
		WithField("room", w.roomId).
		WithField("identity", w.identity).
		Info("Reconnected to LiveKit room")
	appstats.OnParticipantReconnected()
}

func (w *LiveKitWebRTC) validateInitParams() error {
	if w.connStateCallback == nil {
		return fmt.Errorf("connStateCallback is not set")
	}

	if w.flowCallback == nil {
		return fmt.Errorf("flowCallback is not set")
	}

	if w.rec == nil {
		return fmt.Errorf("recorder is not set")
	}

	return nil
}

func (w *LiveKitWebRTC) processKeyframeRequests() {
	defer w.requestKeyframeWg.Done()

	for {
		select {
		case ssrc, ok := <-w.keyframeRequestChan:
			if !ok {
				log.WithField("session", w.ctx.Value("session")).Debug("Keyframe request channel closed, processor shutting down.")
				return
			}
			log.WithField("session", w.ctx.Value("session")).Tracef("Processing PLI request for SSRC %d from channel", ssrc)
			// Locks
			w.RequestKeyframeForSSRC(ssrc)
		case <-w.requestKeyframeCtx.Done():
			log.WithField("session", w.ctx.Value("session")).Debug("Keyframe request processor shutting down due to context cancellation.")
			return
		}
	}
}

func (w *LiveKitWebRTC) connectToRoom() error {
	token, err := w.buildRecorderToken(w.roomId, w.identity)

	if err != nil {
		return fmt.Errorf("failed to build recorder token: %w", err)
	}

	log.WithField("session", w.ctx.Value("session")).
		WithField("room", w.roomId).
		WithField("identity", w.identity).
		Debugf("Connecting to LiveKit room")

	connectStart := time.Now()
	room, err := lksdk.ConnectToRoomWithToken(w.cfg.Host, token,
		&lksdk.RoomCallback{
			ParticipantCallback: lksdk.ParticipantCallback{
				OnTrackSubscribed:         w.onTrackSubscribed,
				OnTrackUnsubscribed:       w.onTrackUnsubscribed,
				OnTrackUnpublished:        w.onTrackUnpublished,
				OnTrackUnmuted:            w.onTrackUnmuted,
				OnTrackMuted:              w.onTrackMuted,
				OnTrackSubscriptionFailed: w.onTrackSubscriptionFailed,
			},
			OnDisconnectedWithReason: w.onDisconnected,
			OnReconnecting:           w.onReconnecting,
			OnReconnected:            w.onReconnected,
		},
		lksdk.WithAutoSubscribe(false),
	)
	appstats.ObserveLiveKitConnectDuration(time.Since(connectStart))

	if err != nil {
		w.Close()
		return fmt.Errorf("failed to connect to LiveKit room: %w", err)
	}

	w.room = room
	log.WithField("session", w.ctx.Value("session")).
		WithField("room", w.roomId).
		WithField("identity", w.identity).
		Infof("Connected to LiveKit room")

	return nil
}

func (w *LiveKitWebRTC) buildRecorderToken(roomName string, identity string) (string, error) {
	f := false
	t := true
	grant := &auth.VideoGrant{
		RoomJoin:       true,
		Room:           roomName,
		CanSubscribe:   &t,
		CanPublish:     &f,
		CanPublishData: &f,
		Hidden:         true,
		Recorder:       true,
	}

	at := auth.NewAccessToken(w.cfg.APIKey, w.cfg.APISecret).
		SetVideoGrant(grant).
		SetIdentity(identity).
		SetKind(livekit.ParticipantInfo_EGRESS).
		SetValidFor(tokenTTL).
		SetMetadata(baseSystemMetadata)

	return at.ToJWT()
}
