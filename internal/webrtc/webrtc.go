package webrtc

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/utils"
	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
	log "github.com/sirupsen/logrus"
)

type NACKTracker struct {
	count     int
	timestamp time.Time
}

type PLITracker struct {
	count     int
	timestamp time.Time
}

type WebRTC struct {
	ctx                 context.Context
	cfg                 config.WebRTC
	pc                  *webrtc.PeerConnection
	rec                 recorder.Recorder
	connStateCallbackFn func(state utils.ConnectionState)
	flowCallback        func(isFlowing bool, videoTimestamp time.Duration, closed bool)
	videoTrackSSRCs     []uint32
	pliStats            map[uint32]PLITracker
	sdpOffer            webrtc.SessionDescription
}

func NewWebRTC(ctx context.Context, cfg config.WebRTC, rec recorder.Recorder) *WebRTC {
	return &WebRTC{
		ctx:                 ctx,
		cfg:                 cfg,
		rec:                 rec,
		sdpOffer:            webrtc.SessionDescription{},
		connStateCallbackFn: nil,
		flowCallback:        nil,
		videoTrackSSRCs:     make([]uint32, 0),
		pliStats:            make(map[uint32]PLITracker),
	}
}

func (w *WebRTC) SetConnectionStateCallback(fn func(state utils.ConnectionState)) {
	w.connStateCallbackFn = fn
}

func (w *WebRTC) SetFlowCallback(fn func(isFlowing bool, videoTimestamp time.Duration, closed bool)) {
	w.flowCallback = fn
}

func (w *WebRTC) SetSDPOffer(offer webrtc.SessionDescription) {
	w.sdpOffer = offer
}

func (w *WebRTC) validateInitParams() error {
	if w.connStateCallbackFn == nil {
		return errors.New("connStateCallbackFn is not set")
	}

	if w.flowCallback == nil {
		return errors.New("flowCallback is not set")
	}

	if w.sdpOffer == (webrtc.SessionDescription{}) {
		return errors.New("sdpOffer is not set")
	}

	return nil
}

func (w *WebRTC) Init() (webrtc.SessionDescription, error) {
	if err := w.validateInitParams(); err != nil {
		return webrtc.SessionDescription{}, err
	}

	cfg := webrtc.Configuration{
		ICEServers:   w.cfg.ICEServers,
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlanWithFallback,
	}

	// Create a MediaEngine object to configure the supported codec
	m := &webrtc.MediaEngine{}

	sdpOffer := sdp.SessionDescription{}

	if err := sdpOffer.Unmarshal([]byte(w.sdpOffer.SDP)); err != nil {
		panic(err)
	}

	// Setup the codecs you want to use.
	// Only support VP8 and OPUS, this makes our WebM muxer code simpler
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, Channels: 0, SDPFmtpLine: "", RTCPFeedback: nil},
		PayloadType:        96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 0, SDPFmtpLine: "", RTCPFeedback: nil},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		panic(err)
	}

	se := &webrtc.SettingEngine{}
	se.SetSRTPReplayProtectionWindow(1024)
	if err := se.SetEphemeralUDPPortRange(w.cfg.RTCMinPort, w.cfg.RTCMaxPort); err != nil {
		panic(err)
	}

	i := &interceptor.Registry{}
	// Use the default set of Interceptors
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		panic(err)
	}

	// Create the API object with the MediaEngine
	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithSettingEngine(*se),
		webrtc.WithInterceptorRegistry(i),
	)

	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(cfg)
	if err != nil {
		panic(err)
	}
	w.pc = peerConnection

	for _, md := range sdpOffer.MediaDescriptions {
		switch md.MediaName.Media {
		case "audio":
			if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
				panic(err)
			}
		case "video":
			if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
				panic(err)
			}
		}
	}

	// Set a handler for when a new remote track starts, this handler copies inbound RTP packets,
	// replaces the SSRC and sends them back
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		isAudio := track.Kind() == webrtc.RTPCodecTypeAudio
		isVideo := track.Kind() == webrtc.RTPCodecTypeVideo

		if isAudio {
			w.rec.SetHasAudio(true)
		} else if isVideo {
			w.rec.SetHasVideo(true)
		}

		log.WithField("session", w.ctx.Value("session")).
			Infof("Track started: kind=%s, codec=%s, payload=%d, ssrc=%d, rid=%s",
				track.Kind().String(), track.Codec().MimeType, track.PayloadType(),
				track.SSRC(), track.RID())

		if isVideo {
			w.videoTrackSSRCs = append(w.videoTrackSSRCs, uint32(track.SSRC()))
			w.pliStats[uint32(track.SSRC())] = PLITracker{count: 0, timestamp: time.Now()}

			if kfr, ok := w.rec.(interface {
				SetKeyframeRequester(recorder.KeyframeRequester)
			}); ok {
				kfr.SetKeyframeRequester(w)
			}

			// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
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
						w.RequestKeyframeForSSRC(uint32(track.SSRC()))
					}
				}
			}()
		}

		rl, err := utils.NewReceiveLog(1024)
		if err != nil {
			panic(err)
		}

		jb := utils.NewJitterBuffer(
			w.cfg.JitterBuffer,
			w.cfg.JitterBufferPktTimeout,
			w.ctx,
		)

		if true {
			senderSSRC := rand.Uint32()
			sentNACKs := 0
			done := make(chan bool)
			defer func() {
				done <- true
			}()
			go func() {
				nackedPackets := make(map[uint16]NACKTracker)   // Track NACK'ed packets
				maxNackAttempts := 10                           // NACK retries - 10 is what mediasoup uses, copy them >:)
				ticker := time.NewTicker(time.Millisecond * 40) // 40ms is also ms

				for {
					select {
					case <-done:
						ticker.Stop()
						return
					case <-ticker.C:
						missing := rl.MissingSeqNumbers(10)

						if len(missing) == 0 {
							continue
						}

						filteredMissing := make([]uint16, 0)
						now := time.Now()

						for _, seq := range missing {
							tracker, exists := nackedPackets[seq]

							// If we've never NACKed this pkt, do it now
							if !exists {
								nackedPackets[seq] = NACKTracker{count: 1, timestamp: now}
								filteredMissing = append(filteredMissing, seq)
								continue
							}

							if tracker.count < maxNackAttempts {
								nackedPackets[seq] = NACKTracker{count: tracker.count + 1, timestamp: now}
								filteredMissing = append(filteredMissing, seq)
							} else if tracker.count == maxNackAttempts {
								log.WithField("session", w.ctx.Value("session")).
									Debugf("Giving up on packet: seq=%d, attempts=%d", seq, maxNackAttempts)

								rl.IgnoreSeqNum(seq)
								jb.SkipPacket(int64(seq))
								nackedPackets[seq] = NACKTracker{count: tracker.count + 1, timestamp: now}
							}
						}

						for seq, tracker := range nackedPackets {
							if now.Sub(tracker.timestamp) > time.Second*5 {
								delete(nackedPackets, seq)
							}
						}

						if len(missing) > 5 {
							log.WithField("session", w.ctx.Value("session")).
								Warnf("Large packet gap detected for SSRC %d: %d packets", senderSSRC, len(missing))
						}

						if len(filteredMissing) > 0 {
							sentNACKs++
							log.WithField("session", w.ctx.Value("session")).
								Debugf("Request NACK for SSRC %d: count=%d, missing=%d, first=%d, last=%d",
									senderSSRC, sentNACKs, len(filteredMissing), filteredMissing[0], filteredMissing[len(filteredMissing)-1])
							nack := &rtcp.TransportLayerNack{
								SenderSSRC: senderSSRC,
								MediaSSRC:  uint32(track.SSRC()),
								Nacks:      utils.NackPairs(filteredMissing),
							}
							errSend := peerConnection.WriteRTCP([]rtcp.Packet{nack})
							if errSend != nil {
								log.WithField("session", w.ctx.Value("session")).
									Error(errSend)
							}
						}
					}
				}
			}()
		}

		var s1, s2 uint16
		var lastTimestamp time.Duration

		if true {
			done := make(chan bool)
			defer func() {
				done <- true
			}()
			go func() {
				ticker := time.NewTicker(time.Millisecond * 100)
				var wasFlowing, isFlowing bool
				for {
					select {
					case <-done:
						ticker.Stop()

						ts := lastTimestamp

						// Use the most recent timestamp from video or audio since
						// we can't differentiate them in status events right now.
						// FIXME this isn't ideal for screen sharing with bundled audio
						if ts == 0 {
							if isVideo {
								ts = w.rec.VideoTimestamp()
							} else {
								ts = w.rec.AudioTimestamp()
							}
						}

						w.flowCallback(false, ts, true)
						return
					case <-ticker.C:
						if s1 == s2 {
							isFlowing = false
						} else {
							isFlowing = true
						}
						s1 = s2
						if isFlowing != wasFlowing {
							ts := lastTimestamp

							// FIXME see comment above
							if ts == 0 {
								if isVideo {
									ts = w.rec.VideoTimestamp()
								} else {
									ts = w.rec.AudioTimestamp()
								}
							}
							w.flowCallback(isFlowing, ts, false)

							if isFlowing {
								ticker.Reset(time.Millisecond * 1000)
							} else {
								ticker.Reset(time.Millisecond * 100)
							}
						}
						wasFlowing = isFlowing
					}
				}
			}()
		}

		var seq int64
		var prevSeq uint16
		packetCounter := 0

		su := utils.NewSequenceUnwrapper(16)
		for {
			// Read RTP packets being sent to Pion
			rtp, _, readErr := track.ReadRTP()
			if readErr != nil {
				if readErr == io.EOF {
					log.WithField("session", w.ctx.Value("session")).
						Infof("%s track stopped", track.Codec().RTPCodecCapability.MimeType)
					return
				}
				log.WithField("session", w.ctx.Value("session")).
					Error(readErr)
				return
			}

			packetCounter++

			if packetCounter > 1 && (rtp.SequenceNumber-prevSeq) > 1 {
				log.WithField("session", w.ctx.Value("session")).
					Debugf("Sequence gap detected: expected=%d, got=%d, diff=%d",
						prevSeq+1, rtp.SequenceNumber, rtp.SequenceNumber-prevSeq-1)
			}
			prevSeq = rtp.SequenceNumber

			seq = su.Unwrap(uint64(rtp.SequenceNumber))

			if !jb.Add(seq, rtp) {
				log.WithField("session", w.ctx.Value("session")).
					Warnf("Failed to add packet to jitter buffer: seq=%d, timestamp=%d",
						rtp.SequenceNumber, rtp.Timestamp)
			}

			rl.Add(rtp.SequenceNumber)

			log.WithField("session", w.ctx.Value("session")).
				Tracef("Processed packet: track=%s, seq=%d, unwrapped=%d, timestamp=%d, size=%d",
					track.ID(), rtp.SequenceNumber, seq, rtp.Timestamp, len(rtp.Payload))

			packets, skipped := jb.NextPackets()

			if packets == nil {
				continue
			}

			log.WithField("session", w.ctx.Value("session")).
				Tracef("Got %d packets from jitter buffer: first=%d, last=%d",
					len(packets), packets[0].SequenceNumber, packets[len(packets)-1].SequenceNumber)

			// Signal packet skip to WebmRecorder to handle potential frame boundary issues
			if skipped {
				skippedSeq, ok := jb.GetAndClearLastSkipped()
				if ok {
					if w.rec != nil {
						w.rec.NotifySkippedPacket(uint16(skippedSeq))
						log.Debugf("Notified recorder of skipped packet: seq=%d", skippedSeq)
					}
				}
			}

			for _, p := range packets {
				s2 = p.SequenceNumber
				switch {
				case isAudio:
					w.rec.PushAudio(p)
					lastTimestamp = w.rec.AudioTimestamp()
				case isVideo:
					w.rec.PushVideo(p)
					lastTimestamp = w.rec.VideoTimestamp()
				}
			}
		}
	})

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		log.WithField("session", w.ctx.Value("session")).
			Infof("webrtc connection state changed: %s", connectionState.String())

		state := utils.NormalizeWebRTCState(connectionState)

		if state.IsTerminalState() {
			if err := peerConnection.Close(); err != nil {
				log.WithField("session", w.ctx.Value("session")).
					Error(err)
			}
		}

		if w.connStateCallbackFn != nil {
			w.connStateCallbackFn(state)
		}
	})

	// Set the remote SessionDescription
	err = peerConnection.SetRemoteDescription(w.sdpOffer)
	if err != nil {
		log.WithField("session", w.ctx.Value("session")).
			Error(err)
		return webrtc.SessionDescription{}, err
	}

	// Create an answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		log.WithField("session", w.ctx.Value("session")).
			Error(err)
		return webrtc.SessionDescription{}, err
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Sets the LocalDescription, and starts our UDP listeners
	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		panic(err)
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete

	// Output the answer in base64
	return *peerConnection.LocalDescription(), nil
}

func (w *WebRTC) RequestKeyframe() {
	for _, ssrc := range w.videoTrackSSRCs {
		w.RequestKeyframeForSSRC(ssrc)
	}
}

func (w WebRTC) RequestKeyframeForSSRC(ssrc uint32) {
	err := w.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}})

	if err != nil {
		log.WithField("session", w.ctx.Value("session")).
			Error(err)
	}

	if _, exists := w.pliStats[ssrc]; !exists {
		w.pliStats[ssrc] = PLITracker{count: 0, timestamp: time.Now()}
	}

	newCount := w.pliStats[ssrc].count + 1
	now := time.Now()
	log.WithField("session", w.ctx.Value("session")).
		Tracef("Sending PLI #%d for SSRC %d (sinceLast=%s)",
			newCount, ssrc, now.Sub(w.pliStats[ssrc].timestamp))
	w.pliStats[ssrc] = PLITracker{count: w.pliStats[ssrc].count + 1, timestamp: time.Now()}
}

func (w *WebRTC) Close() time.Duration {
	if w.pc != nil {
		w.pc.Close()
	}

	if w.rec != nil {
		return w.rec.Close()
	}

	return 0
}

var _ recorder.KeyframeRequester = (*WebRTC)(nil)
