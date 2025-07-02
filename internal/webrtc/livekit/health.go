package livekit

import (
	"context"
	"fmt"
	"time"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/config"
	"github.com/google/uuid"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

const (
	healthCheckTimeout   = 10 * time.Second
	healthSystemMetadata = `{"bbb_system_health_check": true}`
)

type connResult struct {
	room *lksdk.Room
	err  error
}

func buildHealthCheckToken(cfg config.LiveKit, roomName, identity string) (string, error) {
	f := false
	grant := &auth.VideoGrant{
		RoomJoin:       true,
		Room:           roomName,
		CanSubscribe:   &f,
		CanPublish:     &f,
		CanPublishData: &f,
		Hidden:         true,
		Recorder:       false,
	}

	at := auth.NewAccessToken(cfg.APIKey, cfg.APISecret).
		SetVideoGrant(grant).
		SetIdentity(identity).
		SetKind(livekit.ParticipantInfo_EGRESS).
		SetValidFor(cfg.HealthCheck.Interval).
		SetMetadata(healthSystemMetadata)

	return at.ToJWT()
}

func CheckConnectivity(cfg config.LiveKit) error {
	ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
	defer cancel()

	roomName := fmt.Sprintf("health-check-%s", uuid.New().String())
	identity := fmt.Sprintf("health-check-identity-%s", uuid.New().String())

	token, err := buildHealthCheckToken(cfg, roomName, identity)

	if err != nil {
		return fmt.Errorf("could not build livekit token for health check: %w", err)
	}

	done := make(chan connResult, 1)

	go func() {
		room, err := lksdk.ConnectToRoomWithToken(cfg.Host, token, &lksdk.RoomCallback{}, lksdk.WithAutoSubscribe(false))
		done <- connResult{room, err}
	}()

	select {
	case <-ctx.Done():
		return fmt.Errorf("livekit health check timed out after %v", healthCheckTimeout)
	case res := <-done:
		if res.err != nil {
			return fmt.Errorf("could not connect to livekit for health check: %w", res.err)
		}
		res.room.Disconnect()

		return nil
	}
}
