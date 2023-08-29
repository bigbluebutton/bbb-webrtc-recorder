# bbb-webrtc-recorder

### Installation

Install `.deb` package using `apt` or `apt-get`:

```
apt update
apt install ./bbb-webrtc-recorder_x.x-x_amd64.deb
```

It depends on `redis` package which will be installed automatically.

### Configuration

Default configuration file:

```
/etc/bbb-webrtc-recorder/bbb-webrtc-recorder.yml
```

```yaml
# Enable verbose logging for debugging
debug: true

recorder:
  # Directory where the recorder will save files
  directory: /var/lib/bbb-webrtc-recorder
  # File mode permissions for the recording directories (octal)
  dirFileMode: 0700
  # File mode permissions for the recording files (octal)
  fileMode: 0700

pubsub:
  channels:
    # PubSub channel where the recorder will receive messages
    subscribe: to-bbb-webrtc-recorder
    # PubSub channel where the recorder will send messages
    publish: from-bbb-webrtc-recorder
  # The adapter which will be used for PubSub connection
  adapter: redis
  # PubSub adapter-specific configuration
  adapters:
    redis:
      address: :6379
      network: tcp
      #password: foobared

webrtc:
  # UDP port range to be used
  rtcMinPort: 24577
  rtcMaxPort: 32768
  # Jitter Buffer size
  jitterBuffer: 512
  # List of IceServers used for RTC
  iceServers:
    - urls:
        - stun:stun.l.google.com:19302
# Example turn server
#    - urls:
#        - turn:turnserver.example.org:1234
#      username: webrtc
#      credential: turnpassword

# HTTP server for testing
# (should be disabled in production)
http:
  port: 8080
  enable: true
```

Default `env` file used by SystemD service:
    * Note: all environment variables must be prefixed with `BBBRECORDER_`
    * Note: all environment variables must be upper case
    * Note: nested objects are separated by `_` (underscore)
        - Example: `BBBRECORDER_RECORDER_DIRECTORY=/var/lib/bbb-webrtc-recorder` (equivalent of `recorder.directory` in config file)

```
/etc/default/bbb-webrtc-recorder
```

Systemd service:

```
systemctl enable --now bbb-webrtc-recorder
systemctl status bbb-webrtc-recorder
```

Make sure to start `redis`:

```
systemctl enable --now redis
systemctl status redis
```

### Debugging

Examples of how to print out config values:

```
bbb-webrtc-recorder [-c config.yml] --dump http
bbb-webrtc-recorder [-c config.yml] --dump http.port
bbb-webrtc-recorder [-c config.yml] --dump recorder.directory
```

To run in debug mode (verbose logging) either enable debug in configuration file
`debug: true` or run the application with `-d` flag:

```
bbb-webrtc-recorder [-c config.yml] -d
```

Viewing SystemD logs:

```
journalctl -u bbb-webrtc-recorder -f
```

### PubSub events/calls

`startRecording` (SFU -> Recorder)

```json5
{
    id: "startRecording",
    recordingSessionId: <String>, // requester-defined - error out if collision.
    sdp: <String>, // offer
    fileName: <String>, // file name INCLUDING format (.webm)
}
```

`startRecordingResponse` (Recorder -> SFU)

```json5
{
    id: "startRecordingResponse",
    recordingSessionId: <String>, // file name,
    status: "ok" | "failed",
    error: undefined | <String>,
    sdp: <String | undefined>, // answer
    fileName: <String | undefined>, // full path to recording
}
```

`recordingRtpStatusChanged` (Recorder -> SFU)

```json5
{
    id: "recordingRtpStatusChanged", // media started or stopped flowing
    status: "flowing" | "not_flowing",
    recordingSessionId: <String>, // file name
    timestampUTC: <Number>, // latest/trigger frame ts, UTC
    timestampHR: <Number>, //monotonic system time (latest/trigger frame ts),
}
```

`stopRecording` (SFU -> Recorder)

```json5
{
    id: "stopRecording",
    recordingSessionId: <String>, // file name
}
```

`recordingStopped` (Recorder -> SFU)

```json5
{
    id: "recordingStopped",
    recordingSessionId: <String>, // file name
    reason: <String>,
    timestampUTC: <Number>, // last written frame timestamp, UTC, wall clock
    timestampHR:  <Number> // last written frame timestamp, monotonic system time
}
```

getRecorderStatus (* -> Recorer)
```JSON5
{
	id: ‘getRecorderStatus’,
}
```

`recorderStatus` (Recorder -> *)

```JSON5
{
	id: ‘recorderStatus’, // Triggered by getRecorderStatus
	appVersion: <String>, // version of the recorder
	instanceId: <String>, // unique instance id
	timestamp: <Number>, // event generation timestamp
}
```
