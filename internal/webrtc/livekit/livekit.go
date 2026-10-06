package livekit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
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
	"github.com/livekit/media-sdk/jitter"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/interceptor"
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
	// The jitter buffer drops a frame still incomplete this long after its
	// first packet. libwebrtc paces at 1.1x the target rate with
	// transport-wide feedback [1] and lets a VP8 key frame take about 300 ms
	// of that rate [2], so a key frame's packets spread over hundreds of ms,
	// and its pacer queue may reach 2 s [3]. The cost: an unrecoverable video
	// loss holds the track that long, though the track buffer usually
	// requests its key frame much sooner.
	//
	// [1] https://webrtc.googlesource.com/src/+/ac9591c580ac697e7bb604debf8a141b18626204/modules/congestion_controller/goog_cc/goog_cc_network_control.cc#58
	// [2] https://webrtc.googlesource.com/src/+/ac9591c580ac697e7bb604debf8a141b18626204/modules/video_coding/codecs/vp8/libvpx_vp8_encoder.cc#927
	// [3] https://webrtc.googlesource.com/src/+/ac9591c580ac697e7bb604debf8a141b18626204/modules/pacing/pacing_controller.h#92
	videoJitterLatency = 2 * time.Second
	audioJitterLatency = 200 * time.Millisecond
	// Each keyframe request also stops LiveKit's NACK repair on the downtrack
	// until it forwards a key frame. Must stay below videoJitterLatency: the
	// key frame the last request brings then follows every loss it suppresses.
	lossKeyframeInterval = time.Second
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
	jitterBuffers      map[string]*trackBuffer
	videoBuffers       map[uint32]*trackBuffer // SSRC -> video track buffer
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

	// constraintMu is held from computing a track's quality until it is
	// sent via sendVideoQuality. Taken before m; constraints itself is guarded by m.
	constraintMu sync.Mutex
	constraints  *constraintBook
	// sendVideoQuality sends a track's subscribed quality to LiveKit
	sendVideoQuality func(trackID string, pub *lksdk.RemoteTrackPublication, quality livekit.VideoQuality)
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
		jitterBuffers:         make(map[string]*trackBuffer),
		videoBuffers:          make(map[uint32]*trackBuffer),
		flowState:             make(map[string]*trackFlowState),
		trackStats:            make(map[string]*appstats.AdapterTrackStats),
		participantIDs:        make(map[string]string),
		rtpWriters:            make(map[string]*recorder.RTPWriter),
		startTs:               time.Now(),
		keyframeRequestChan:   make(chan uint32, 100),
		requestKeyframeCtx:    requestKeyframeCtx,
		requestKeyframeCancel: requestKeyframeCancel,
		pendingSubscriptions:  make(map[string]time.Time),
		constraints:           newConstraintBook(cfg.PreferredVideoQuality),
		sendVideoQuality:      sendPubVideoQuality,
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
	w.StopConstraints()

	if w.room != nil {
		w.room.Disconnect()
	}

	if w.requestKeyframeCancel != nil {
		w.requestKeyframeCancel()
	}

	w.requestKeyframeWg.Wait()

	w.m.Lock()
	for trackID := range w.rtpWriters {
		w.closeRTPDump(trackID, log.WithField("session", w.ctx.Value("session")).WithField("trackID", trackID))
	}

	w.rtpWriters = nil
	w.m.Unlock()

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

	jitterBuffers := make(map[string]*trackBuffer, len(w.jitterBuffers))
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

	constrained, constraintRecords := w.constraints.snapshot(time.Now())

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
		Constrained:         constrained,
		Constraints:         constraintRecords,
	}

	for trackID, remoteTrackPub := range remoteTrackPubs {
		mimeType := remoteTrackPub.MimeType()

		var bufferStatsData *jitter.BufferStats

		if jb, ok := jitterBuffers[trackID]; ok && jb != nil {
			bufferStatsData = jb.stats()
		} else {
			bufferStatsData = &jitter.BufferStats{}
		}

		// PacketsDropped counts only packets that arrived; missing packets the
		// buffer stopped waiting for are PacketsLost. Both feed the buffer loss
		// metric.
		packetsDropped := bufferStatsData.PacketsDropped + bufferStatsData.PacketsLost

		currentTrackAdapterStats := trackStats[trackID]

		pliCount := 0
		if remoteTrackPub.Kind() == lksdk.TrackKindVideo {
			for _, tracker := range pliStats {
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
				PacketsDropped: packetsDropped,
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

// KeyframeRequestedSince answers for the video track buffer of ssrc, from
// its own loss reports. While the track flows, the read loop pushes a packet
// at least every PacketReadTimeout.
func (w *LiveKitWebRTC) KeyframeRequestedSince(ssrc uint32, since time.Time) bool {
	w.m.Lock()
	buffer := w.videoBuffers[ssrc]
	w.m.Unlock()

	return buffer != nil && buffer.requestedSince(since, w.cfg.PacketReadTimeout)
}

func (w *LiveKitWebRTC) HasTrack(trackID string) bool {
	w.m.Lock()
	defer w.m.Unlock()

	return slices.Contains(w.trackIds, trackID)
}

// ApplyConstraint caps the video quality of the capture tracks whose
// source c lists, including tracks subscribed later. Idempotent per
// reason: an active reason must be lifted before it can be changed. It
// reports whether it lowered the quality of at least one track.
func (w *LiveKitWebRTC) ApplyConstraint(c types.CaptureConstraint) bool {
	logger := log.WithFields(log.Fields{
		"session":      w.ctx.Value("session"),
		"reason":       c.Reason,
		"videoQuality": c.VideoQuality.String(),
		"sources":      sourceNames(c.Sources),
	})

	if c.Reason == "" || c.VideoQuality < livekit.VideoQuality_LOW || c.VideoQuality > livekit.VideoQuality_HIGH {
		logger.Warn("Ignoring invalid capture constraint")

		return false
	}

	w.constraintMu.Lock()
	defer w.constraintMu.Unlock()

	w.m.Lock()
	ev, noop := w.constraints.apply(c, time.Now())
	pubs := w.constrainedPubs(ev.Tracks)
	w.m.Unlock()

	if noop != "" {
		logger.Debugf("Capture constraint not applied: %s", noop)

		return false
	}

	w.setVideoQualities(pubs, ev.Tracks)
	w.onConstraintApplied(ev, "mid_capture")

	return qualityChanged(ev.Tracks)
}

// LiftConstraint removes the constraint of the given reason and restores
// the quality of the tracks it capped. It reports whether it raised the
// quality of at least one track.
func (w *LiveKitWebRTC) LiftConstraint(reason string) bool {
	w.constraintMu.Lock()
	defer w.constraintMu.Unlock()

	w.m.Lock()
	ev, noop := w.constraints.lift(reason, time.Now())
	pubs := w.constrainedPubs(ev.Tracks)
	w.m.Unlock()

	if noop != "" {
		log.WithField("session", w.ctx.Value("session")).
			WithField("reason", reason).
			Debugf("Capture constraint not lifted: %s", noop)

		return false
	}

	w.setVideoQualities(pubs, ev.Tracks)
	duration := time.Duration(ev.Record.EndTimeMs-ev.Record.StartTimeMs) * time.Millisecond

	for _, tq := range ev.Tracks {
		log.WithFields(log.Fields{
			"session":             w.ctx.Value("session"),
			"reason":              reason,
			"trackId":             tq.TrackID,
			"source":              tq.Source.String(),
			"fromQuality":         tq.From.String(),
			"toQuality":           tq.To.String(),
			"constrainedDuration": duration.String(),
		}).Infof("Capture constraint lifted, video quality %s -> %s after %s", tq.From, tq.To, duration)
	}

	appstats.OnCaptureConstraintLifted(reason)

	return qualityChanged(ev.Tracks)
}

// setSubscribeVideoQuality registers pub into the constraints book and
// computes its target quality at subscription time.
func (w *LiveKitWebRTC) setSubscribeVideoQuality(trackID string, source livekit.TrackSource, pub *lksdk.RemoteTrackPublication) {
	w.constraintMu.Lock()
	defer w.constraintMu.Unlock()

	w.m.Lock()
	w.remoteTrackPubs[trackID] = pub
	quality, events := w.constraints.addTrack(trackID, source, time.Now())
	w.m.Unlock()

	log.WithField("session", w.ctx.Value("session")).
		Debugf("Setting video quality to %s for track %s", quality, trackID)
	w.sendVideoQuality(trackID, pub, quality)

	for _, ev := range events {
		w.onConstraintApplied(ev, "subscribe")
	}
}

// constrainedPubs must be called with m held.
func (w *LiveKitWebRTC) constrainedPubs(tracks []trackQuality) map[string]*lksdk.RemoteTrackPublication {
	pubs := make(map[string]*lksdk.RemoteTrackPublication, len(tracks))

	for _, tq := range tracks {
		if pub, ok := w.remoteTrackPubs[tq.TrackID]; ok {
			pubs[tq.TrackID] = pub
		}
	}

	return pubs
}

// setVideoQualities must be called with constraintMu held and m released:
// SetVideoQuality writes to the signal connection.
func (w *LiveKitWebRTC) setVideoQualities(pubs map[string]*lksdk.RemoteTrackPublication, tracks []trackQuality) {
	for _, tq := range tracks {
		if pub, ok := pubs[tq.TrackID]; ok && tq.From != tq.To {
			w.sendVideoQuality(tq.TrackID, pub, tq.To)
		}
	}
}

func sendPubVideoQuality(_ string, pub *lksdk.RemoteTrackPublication, quality livekit.VideoQuality) {
	// Ignore error - only throws when video quality = OFF which we do not care about
	_ = pub.SetVideoQuality(quality)
}

func (w *LiveKitWebRTC) onConstraintApplied(ev constraintEvent, phase string) {
	for _, tq := range ev.Tracks {
		log.WithFields(log.Fields{
			"session":     w.ctx.Value("session"),
			"reason":      ev.Reason,
			"trackId":     tq.TrackID,
			"source":      tq.Source.String(),
			"fromQuality": tq.From.String(),
			"toQuality":   tq.To.String(),
			"phase":       phase,
		}).Infof("Capture constraint applied, video quality %s -> %s", tq.From, tq.To)
	}

	if ev.Opened {
		appstats.OnCaptureConstrained(ev.Reason, ev.FirstForReason)
	}
}

// StopConstraints closes the constraint intervals still open and makes
// later ApplyConstraint/LiftConstraint calls no-ops, so stats taken after
// it no longer change. Close calls it too; only the first call acts.
func (w *LiveKitWebRTC) StopConstraints() {
	w.m.Lock()
	records := w.constraints.stop(time.Now())
	w.m.Unlock()

	for _, record := range records {
		duration := time.Duration(record.EndTimeMs-record.StartTimeMs) * time.Millisecond

		log.WithFields(log.Fields{
			"session":             w.ctx.Value("session"),
			"reason":              record.Reason,
			"videoQuality":        record.VideoQuality,
			"constrainedDuration": duration.String(),
		}).Infof("Capture stopped while constrained, video quality %s for %s", record.VideoQuality, duration)
		appstats.OnConstrainedCaptureStopped(record.Reason)
	}
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

	if len(pendingSubscriptions) == 0 {
		return nil, fmt.Errorf("no tracks available")
	}

	w.m.Lock()
	maps.Copy(w.remoteTrackPubs, subscribedTrackPubs)
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
			w.setSubscribeVideoQuality(pub.SID(), pub.Source(), pub)
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

func jitterLatencyFor(kind TrackKind) time.Duration {
	if kind == TrackKindVideo {
		return videoJitterLatency
	}

	return audioJitterLatency
}

// registerTrackBuffer wires a track's buffer into the adapter, apart from
// addJitterBuffer so that the writer tests register buffers of their own:
// without the SSRC entry, the writer repeats every request the buffer made.
func (w *LiveKitWebRTC) registerTrackBuffer(trackID string, isVideo bool, ssrcForHandler uint32, buffer *trackBuffer) {
	w.m.Lock()
	w.jitterBuffers[trackID] = buffer

	if isVideo {
		if _, exists := w.pliStats[ssrcForHandler]; !exists {
			w.pliStats[ssrcForHandler] = pliTracker{count: 0, timestamp: time.Now()}
		}
		w.videoBuffers[ssrcForHandler] = buffer
	}
	w.m.Unlock()

	if isVideo {
		if kfr, ok := w.rec.(interface {
			SetKeyframeRequester(interfaces.KeyframeRequester)
		}); ok {
			kfr.SetKeyframeRequester(w)
		}
	}
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
		w.openRTPDump(trackID)
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

	var ssrcForHandler uint32

	if track != nil {
		ssrcForHandler = uint32(track.SSRC())
	}

	buffer := w.addJitterBuffer(trackID, depacketizer, jitterLatencyFor(trackKind), ssrcForHandler, isVideo && track != nil)

	appstats.OnTrackRecordingStarted(string(trackKind), string(mimeType), pub.Source().String())

	go w.readTrack(track, trackInfo{
		id:      trackID,
		kind:    trackKind,
		mime:    mimeType,
		source:  pub.Source().String(),
		ssrc:    ssrcForHandler,
		pubMime: pub.MimeType(),
	}, buffer)
}

// trackFilePath is <base>.<trackID><suffix> next to the recording at
// recordingPath, with any character of trackID unsafe in a file name replaced.
func trackFilePath(recordingPath, trackID, suffix string) string {
	safeID := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, trackID)

	return strings.TrimSuffix(recordingPath, filepath.Ext(recordingPath)) + "." + safeID + suffix
}

// openRTPDump starts trackID's rtpdump file, <base>.<trackID>.rtp next to
// the recording, for readTrack to write the track's packets to.
func (w *LiveKitWebRTC) openRTPDump(trackID string) {
	logger := log.WithField("session", w.ctx.Value("session")).WithField("trackID", trackID)

	// A track subscribed again reuses the path: its earlier dump must stop
	// writing before the file is truncated.
	w.m.Lock()
	w.closeRTPDump(trackID, logger)
	w.m.Unlock()

	rtpPath := trackFilePath(w.rec.GetFilePath(), trackID, ".rtp")
	rtpWriter, err := recorder.NewRTPWriter(rtpPath, net.IP{0, 0, 0, 0}, 0)

	if err != nil {
		logger.Errorf("Failed to create RTP writer for track %s: %v", trackID, err)

		return
	}

	w.m.Lock()
	// A subscription can land after Close, which disconnects the room but
	// not the pion OnTrack goroutines already running.
	if w.rtpWriters == nil {
		w.m.Unlock()
		_ = rtpWriter.Close()

		return
	}
	w.closeRTPDump(trackID, logger)
	w.rtpWriters[trackID] = rtpWriter
	w.m.Unlock()

	logger.Infof("RTP dump enabled for track %s to %s", trackID, rtpPath)
}

// closeRTPDump closes and forgets trackID's rtpdump writer, if any. Call it
// with w.m held.
func (w *LiveKitWebRTC) closeRTPDump(trackID string, logger *log.Entry) {
	writer, ok := w.rtpWriters[trackID]
	if !ok {
		return
	}

	delete(w.rtpWriters, trackID)

	if err := writer.Close(); err != nil {
		logger.Warnf("Failed to close rtp writer for track %s: %v", trackID, err)
	}
}

// watchFlow marks the track not flowing once a tick finds no media packet
// read since the previous one, until done is closed.
func (w *LiveKitWebRTC) watchFlow(trackID string, done <-chan struct{}) {
	ticker := time.NewTicker(notFlowingTicker)
	var lastSeqNum uint16
	for {
		select {
		case <-done:
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
}

// trackSource is the part of *webrtc.TrackRemote the read loop uses.
type trackSource interface {
	ReadRTP() (*rtp.Packet, interceptor.Attributes, error)
	SetReadDeadline(time.Time) error
}

type trackInfo struct {
	id     string
	kind   TrackKind
	mime   MimeType // the negotiated codec's, lowercased
	source string
	ssrc   uint32
	// The publication's MIME type as LiveKit signals it ("video/VP8"), which
	// the read-error metric and the stop logs carry.
	pubMime string
}

// readTrack feeds the recorder from src until the track ends, then closes
// buffer and reports the connection closed, or failed on a panic.
func (w *LiveKitWebRTC) readTrack(src trackSource, info trackInfo, buffer *trackBuffer) {
	defer buffer.close()
	defer func() {
		appstats.OnTrackRecordingStopped(string(info.kind), string(info.mime), info.source)
		log.WithField("session", w.ctx.Value("session")).
			WithField("trackID", info.id).
			WithField("source", info.source).
			WithField("kind", info.kind).
			WithField("mime", info.mime).
			WithField("ssrc", info.ssrc).
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

	flowCheckDone := make(chan struct{})
	defer close(flowCheckDone)
	go w.watchFlow(info.id, flowCheckDone)

	w.m.Lock()
	rtpWriter, rtpWriterExists := w.rtpWriters[info.id]
	w.m.Unlock()

	writePackets := func(packets []*rtp.Packet) {
		for _, p := range packets {
			switch info.kind {
			case TrackKindVideo:
				w.rec.PushVideo(p)
			case TrackKindAudio:
				w.rec.PushAudio(p)
			}
		}
	}

	push := w.packetPusher(info.id, buffer)

	for {
		readDeadline := time.Now().Add(w.cfg.PacketReadTimeout)
		// Ignore error from SetReadDeadline - it comes from pion/packetio
		// but it'll never throw - probably conforming to some interface
		_ = src.SetReadDeadline(readDeadline)
		packet, _, err := src.ReadRTP()

		if err != nil {
			if procErr := w.handleReadRTPError(err, info); procErr != nil {
				if procErr == io.EOF {
					log.WithField("session", w.ctx.Value("session")).
						Infof("%s track=%s stopped", info.pubMime, info.id)

					// No later packet hands out what the jitter buffer holds.
					if tail := buffer.flush(); len(tail) > 0 {
						writePackets(tail)
						w.processPacketStats(info.id, tail)
					}
				} else {
					log.WithField("session", w.ctx.Value("session")).
						Errorf("Unexpected error handling RTP packet from track %s: %v", info.id, procErr)
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
					WithField("trackID", info.id).
					Warnf("failed to write RTP packet for track %s: %v", info.id, err)
			}
		}

		packets := push(packet)

		if len(packets) == 0 {
			continue
		}

		writePackets(packets)
		w.processPacketStats(info.id, packets)
	}
}

// packetPusher returns the push the read loop hands trackID's packets to.
// The flow state follows their arrival, not buffer's release: the SFU stamps
// the recording's start when the first flowing event reaches it, and buffer
// can hold a video frame, and the frames behind it, for its whole latency.
// Padding, which LiveKit sends a Go subscriber's video until its first key
// frame, is not media arriving, nor is a duplicate.
func (w *LiveKitWebRTC) packetPusher(trackID string, buffer *trackBuffer) func(*rtp.Packet) []*rtp.Packet {
	var arrived seqWindow

	return func(packet *rtp.Packet) []*rtp.Packet {
		if len(packet.Payload) > 0 && !arrived.duplicate(packet.SequenceNumber) {
			w.updateFlowState(trackID, packet.SequenceNumber, time.Now())
		}

		return buffer.push(packet)
	}
}

// addJitterBuffer creates and registers the jitter buffer of trackID. m and
// a buffer's lock are never held together, and the buffer calls its loss
// handler with its lock held, also on its timer goroutine outside the read
// loop's recover, so the handler only queues the keyframe request.
func (w *LiveKitWebRTC) addJitterBuffer(
	trackID string,
	depacketizer rtp.Depacketizer,
	latency time.Duration,
	ssrc uint32,
	requestKeyframes bool,
) *trackBuffer {
	buffer := newTrackBuffer(depacketizer, latency, lossKeyframeInterval, func() {
		if requestKeyframes {
			w.queueKeyframeRequest(ssrc, "packet_drop")
		}
	})

	w.registerTrackBuffer(trackID, requestKeyframes, ssrc, buffer)

	return buffer
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

func (w *LiveKitWebRTC) trackReadErrorStats(err error, info trackInfo) {
	if err != io.EOF {
		appstats.OnRTPReadError(info.source, string(info.kind), info.pubMime, err.Error())
		w.m.Lock()
		w.trackStats[info.id].RTPReadErrors++
		w.m.Unlock()
	}
}

func (w *LiveKitWebRTC) handleReadRTPError(err error, info trackInfo) error {
	trackID := info.id
	var netErr net.Error

	// Other tracks add their entries concurrently.
	w.m.Lock()
	var flowState *trackFlowState
	if state, ok := w.flowState[trackID]; ok {
		snapshot := *state
		flowState = &snapshot
	}
	w.m.Unlock()

	log.WithField("session", w.ctx.Value("session")).
		Tracef("Error reading RTP packet from track %s: %+v", trackID, err)
	w.trackReadErrorStats(err, info)

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
			Infof("%s track stopped", info.pubMime)
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
	if !w.hasRemoteTrackPub(trackID) {
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
	if !w.hasRemoteTrackPub(trackID) {
		return
	}

	log.WithField("session", w.ctx.Value("session")).
		Infof("Track muted: %s", trackID)
}

func (w *LiveKitWebRTC) hasRemoteTrackPub(trackID string) bool {
	w.m.Lock()
	defer w.m.Unlock()

	_, exists := w.remoteTrackPubs[trackID]

	return exists
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
