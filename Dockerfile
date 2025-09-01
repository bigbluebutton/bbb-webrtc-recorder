# Build stage
FROM golang:1.25 as builder

WORKDIR /app

COPY go.* ./

RUN go mod tidy

COPY . ./

RUN APP_VERSION=$(cat ./VERSION | sed 's/ /-/g') \
      go build -o ./build/bbb-webrtc-recorder \
      -ldflags="-X 'github.com/bigbluebutton/bbb-webrtc-recorder/internal.AppVersion=v${APP_VERSION}'" \
      ./cmd/bbb-webrtc-recorder


RUN mv /app/build/bbb-webrtc-recorder /usr/bin/bbb-webrtc-recorder

# Running stage
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y gosu

# use same UID as in the recordings container
RUN groupadd -g 998 bigbluebutton && useradd -m -u 998 -g bigbluebutton bigbluebutton

# Copy the binary to the production image from the builder stage.
COPY --from=builder /usr/bin/bbb-webrtc-recorder /usr/bin/bbb-webrtc-recorder
COPY --from=builder /app/config/bbb-webrtc-recorder.yml /etc/bbb-webrtc-recorder/bbb-webrtc-recorder.yml

CMD ["/bin/sh", "-c", "chown -R bigbluebutton:bigbluebutton /var/lib/bbb-webrtc-recorder && gosu bigbluebutton /usr/bin/bbb-webrtc-recorder"]
