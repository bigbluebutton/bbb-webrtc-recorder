package webrtc

import (
	"context"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/recorder"
	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/webrtc/utils"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	log "github.com/sirupsen/logrus"
	"io"
	"math/rand"
	"time"
)

type WebRTC struct {
	ctx context.Context
	cfg config.WebRTC
	pc  *webrtc.PeerConnection
}

func NewWebRTC(ctx context.Context, cfg config.WebRTC) *WebRTC {
	return &WebRTC{ctx: ctx, cfg: cfg}
}

func (w WebRTC) Init(offer webrtc.SessionDescription, r recorder.Recorder, connStateCallbackFn func(state webrtc.ICEConnectionState)) webrtc.SessionDescription {
	// Prepare the configuration
	cfg := webrtc.Configuration{
		ICEServers:   w.cfg.ICEServers,
		SDPSemantics: webrtc.SDPSemanticsUnifiedPlanWithFallback,
	}

	// Create a MediaEngine object to configure the supported codec
	m := &webrtc.MediaEngine{}

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

	// Create the API object with the MediaEngine
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(*se))

	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(cfg)
	if err != nil {
		panic(err)
	}
	w.pc = peerConnection

	//done := make(chan bool)
	trackDone := make([]chan bool, 0)
	// Set a handler for when a new remote track starts, this handler copies inbound RTP packets,
	// replaces the SSRC and sends them back
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
		ticker := time.NewTicker(time.Second * 3)
		done1 := make(chan bool)
		trackDone = append(trackDone, done1)
		go func() {
			for {
				select {
				case <-done1:
					ticker.Stop()
					return
				case <-ticker.C:
					errSend := peerConnection.WriteRTCP([]rtcp.Packet{
						&rtcp.PictureLossIndication{
							MediaSSRC: uint32(track.SSRC()),
						},
					})
					if errSend != nil {
						log.WithField("session", w.ctx.Value("session")).
							Error(errSend)
					}
				}
			}
		}()

		receiveLog, err := utils.NewReceiveLog(1024)
		if err != nil {
			panic(err)
		}
		var senderSSRC = rand.Uint32()
		done2 := make(chan bool)
		trackDone = append(trackDone, done2)
		go func() {
			ticker := time.NewTicker(time.Millisecond * 50)
			for {
				select {
				case <-done2:
					ticker.Stop()
					return
				case <-ticker.C:
					missing := receiveLog.MissingSeqNumbers(10)
					nack := &rtcp.TransportLayerNack{
						SenderSSRC: senderSSRC,
						MediaSSRC:  uint32(track.SSRC()),
						Nacks:      utils.NackPairs(missing),
					}
					errSend := peerConnection.WriteRTCP([]rtcp.Packet{nack})
					if errSend != nil {
						log.WithField("session", w.ctx.Value("session")).
							Error(errSend)
					}
				}
			}
		}()

		log.WithField("session", w.ctx.Value("session")).
			Infof("%s (%d) track started", track.Codec().RTPCodecCapability.MimeType, track.PayloadType())
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

			receiveLog.Add(rtp.SequenceNumber)

			r.Push(rtp, track)
		}
	})
	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		log.WithField("session", w.ctx.Value("session")).
			Infof("webrtc connection state changed: %s", connectionState.String())
		if connectionState > webrtc.ICEConnectionStateConnected {
			if err := peerConnection.Close(); err != nil {
				log.WithField("session", w.ctx.Value("session")).
					Error(err)
			}

			for _, done := range trackDone {
				done <- true
			}
			trackDone = nil
		}
		connStateCallbackFn(connectionState)
	})

	// Set the remote SessionDescription
	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		log.WithField("session", w.ctx.Value("session")).
			Error(err)
		return webrtc.SessionDescription{}
	}

	// Create an answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		log.WithField("session", w.ctx.Value("session")).
			Error(err)
		return webrtc.SessionDescription{}
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
	return *peerConnection.LocalDescription()
}

func (w WebRTC) Close() error {
	return w.pc.Close()
}
