# Build stage
FROM golang:1.19 as builder

ARG APP_VERSION=devel
ARG GOMOD=github.com/bigbluebutton/bbb-webrtc-recorder

WORKDIR /app

COPY go.* ./

RUN go mod tidy

COPY . ./

RUN go build -o ./build/bbb-webrtc-recorder \
      -ldflags="-X '${GOMOD}/internal.AppVersion=${APP_VERSION}'" \
      ./cmd/bbb-webrtc-recorder

RUN mv /app/build/bbb-webrtc-recorder /usr/bin/bbb-webrtc-recorder

RUN rm -rf /app

# Running stage
FROM debian:bookworm-slim

# Copy the binary to the production image from the builder stage.
COPY --from=builder /usr/bin/bbb-webrtc-recorder /usr/bin/bbb-webrtc-recorder

CMD ["/usr/bin/bbb-webrtc-recorder"]
