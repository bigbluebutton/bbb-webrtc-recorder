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
log:
  level: INFO  # TRACE, DEBUG, INFO, WARN, ERROR, FATAL

recorder:
  # Directory where the recorder will save files
  directory: /var/lib/bbb-webrtc-recorder
  # File mode permissions for the recording directories (octal)
  dirFileMode: 0700
  # File mode permissions for the recording files (octal)
  fileMode: 0700
  # Whether to write to /dev/null instead of a file (for testing)
  writeToDevNull: false
  # Whether an IVF copy of WebM recordings should be generated
  writeIVFCopy: false
  # Write a stats file for each recording
  writeStatsFile: false
  videoPacketQueueSize: 256
  audioPacketQueueSize: 32
  useCustomSampler: true

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

constraints:
  # Caps the video quality of LiveKit captures while the recordings
  # filesystem is low on free space.
  diskPressure:
    enable: false
    # Filesystem to watch; empty means recorder.directory
    path: ""
    checkInterval: 15s
    # Engage when free space is below EITHER level; 0 disables a criterion.
    # Levels are whole numbers: MiB, and percents of the filesystem size.
    engageFreeMiB: 20480     # MiB
    engageFreePercent: 10    # percent of the filesystem size
    # Release when free space is at or above EVERY enabled release level
    releaseFreeMiB: 25600    # MiB
    releaseFreePercent: 15   # percent of the filesystem size
    # LOW | MEDIUM | HIGH
    videoQuality: LOW
    # LiveKit track sources to cap: camera, screen_share
    sources: [camera]
```

Default `env` file used by SystemD service:
    * Note: all environment variables must be prefixed with `BBBRECORDER_`
    * Note: all environment variables must be upper case
    * Note: nested objects are separated by `_` (underscore)
        - Example: `BBBRECORDER_RECORDER_DIRECTORY=/var/lib/bbb-webrtc-recorder` (equivalent of `recorder.directory` in config file)
    * Note: lists are comma-separated
        - Example: `BBBRECORDER_CONSTRAINTS_DISKPRESSURE_SOURCES=camera,screen_share` (equivalent of `constraints.diskPressure.sources` in config file)

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
bbb-webrtc-recorder [-c config.yml] --dump constraints.diskPressure
```

Viewing SystemD logs:

```
journalctl -u bbb-webrtc-recorder -f
```

[Trials and tribulations](docs/trials-and-tribulations.md) covers recording
scenarios that are hard to get right, such as a microphone whose media clock
runs slow. For each one, it says what goes wrong, what the recorder does,
where to look and its known limits.

### PubSub events/calls

`startRecording` (SFU -> Recorder)

```json5
{
    id: 'startRecording',
    recordingSessionId: <String>, // requester-defined - error out if collision
    fileName: <String>, // file name INCLUDING format (.webm)
    adapter: <String>, // "mediasoup" or "livekit" - defaults to "mediasoup" if not specified
    adapterOptions: {
        // mediasoup-specific options
        mediasoup?: {
            sdp: <String>, // required for mediasoup adapter if legacy sdp is not provided
        },
        // LiveKit-specific options
        livekit?: {
            room: <String>, // required for livekit adapter
            trackIds: <String[]>, // required for livekit adapter - array of track IDs to record
        }
    },
    // Legacy field for backward compatibility
    sdp?: <String>, // offer - required for mediasoup adapter if adapterOptions.mediasoup.sdp is not provided
    metadata?: <Object>, // Opaque, client-defined metadata. Returned in startRecordingResponse and getRecordingsResponse
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
    metadata: <Object | undefined>, // Opaque metadata from the original startRecording request
}
```

`recordingRtpStatusChanged` (Recorder -> SFU)

```json5
{
    id: "recordingRtpStatusChanged", // media started or stopped flowing
    status: "flowing" | "not_flowing",
    recordingSessionId: <String>, // file name
    timestampUTC: <String>, // when the event was built, RFC 3339
    timestampHR: <Number>, // LiveKit: ms since the session began; mediasoup: the WebM's media time
}
```

`recordingMediaStarted` (Recorder -> SFU), LiveKit sessions only

```json5
{
    id: "recordingMediaStarted", // once the media start is settled, or 4.5 s old
    recordingSessionId: <String>,
    mediaStartUTC: <Number>, // when the first frame written arrived, less any backlog (at most 1 s), epoch ms
    mediaStartHR: <Number>, // the same time on the host's CLOCK_MONOTONIC, ms
}
```

See [Media timestamps](#media-timestamps).

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
    timestampUTC: <String>, // when the event was built, RFC 3339
    timestampHR?: <Number>, // the WebM's video duration, ms; absent without video
    // LiveKit sessions that wrote a frame
    mediaStartUTC?: <Number>, // as in recordingMediaStarted, unless the start moved after it was sent
    mediaStartHR?: <Number>,
    mediaEndUTC?: <Number>, // mediaStart plus the media written, to the end of the last frame
    mediaEndHR?: <Number>,
}
```

`getRecorderStatus` (* -> Recorder)
```JSON5
{
	id: 'getRecorderStatus',
}
```

`recorderStatus` (Recorder -> *)

```JSON5
{
	id: 'recorderStatus', // Triggered by getRecorderStatus
	appVersion: <String>, // version of the recorder
	instanceId: <String>, // unique instance id
	timestamp: <Number>, // event generation timestamp
	capabilities: <String[]>, // "mediaTimestamps": see recordingMediaStarted
}
```

`getRecordings` (* -> Recorder)
```json5
{
    "id": "getRecordings",
    "requestId": "<String>" // requester-defined - for request/response correlation
}
```

`getRecordingsResponse` (Recorder -> *)
```json5
{
    "id": "getRecordingsResponse",
    "requestId": "<String>", // Mirrors the requestId from the getRecordings request
    "recordings": [
        {
            "recordingSessionId": "<String>",
            "fileName": "<String>",
            "adapter": "<String>", // "mediasoup" or "livekit"
            "metadata": { ... }, // Opaque metadata from the original startRecording request
            "adapterOptions": {
                // mediasoup-specific options
                "mediasoup": {
                    "sdp": "<String>"
                },
                // LiveKit-specific options
                "livekit": {
                    "room": "<String>",
                    "trackIds": ["<String>"]
                }
            },
            "startTimeUTC": "<Number | undefined>", // epoch ms at the first flowing event
            "startTimeHR": "<Number | undefined>", // LiveKit: ns from the session's start to that event; mediasoup: the WebM's media time
            "mediaStartUTC": "<Number | undefined>", // LiveKit: as in recordingMediaStarted, once sent
            "mediaStartHR": "<Number | undefined>"
        }
    ]
}
```

#### Media timestamps

record-and-playback places the first frame of each recorded file at the
`timestamp` of its start event, then follows the file's own timestamps. That
field is the host's CLOCK_MONOTONIC in milliseconds, the clock akka-apps and
bbb-webrtc-sfu use for every recording event. For LiveKit sessions, the
recorder measures that time so the controller does not have to guess it:

- `mediaStart` is when the first frame written reached the recorder: the
  arrival of its earliest packet, before the jitter buffer.

  The first reads can find frames that waited in the receive buffers, and a
  camera can send its first frames late and catch up. So a frame that
  arrived sooner than its timestamp implies moves `mediaStart` back, by at
  most 1 s. Only frames written within a track's first 2 s of media can do
  this. A delay on the path that lasts longer stays in `mediaStart`, and the
  file plays that much late.

  A step over 120 ms between a microphone's frames closes that window
  early. A video track's steps do not: a camera catching up can send frames
  700 ms apart.
- `mediaEnd` is `mediaStart` plus the media written after the first frame, to
  the end of the last frame. So `mediaEnd - mediaStart` is the file's
  duration.

Each comes as epoch ms (`*UTC`) and CLOCK_MONOTONIC ms (`*HR`) for the same
instant. The older `timestampUTC`, `timestampHR`, `startTimeUTC` and
`startTimeHR` fields keep their form for older controllers. None of them is in
the recording events' clock.
