FROM golang:1.19-alpine

COPY . /app
WORKDIR /app

ENV APP_VERSION $(cat ./VERSION)
ENV GOMOD $(go list -m)

RUN go mod tidy
RUN go build -o ./build/bbb-webrtc-recorder ./cmd/bbb-webrtc-recorder

RUN ls -ahlt ./*

RUN mv /app/build/bbb-webrtc-recorder /usr/local/bin/bbb-webrtc-recorder

WORKDIR /usr/local/bin

RUN rm -rf /app

EXPOSE 8080

RUN ls -ahlt
CMD ["./bbb-webrtc-recorder"]
