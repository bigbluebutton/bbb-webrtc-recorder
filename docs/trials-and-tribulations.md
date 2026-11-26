# Trials and tribulations

Recording scenarios that are hard to get right: senders and networks that
misbehave, and edge cases the code handles on purpose. Read a scenario when a
recording comes out wrong, or before you change the code that handles it.

Each scenario is one section with the same four parts: what goes wrong, what
the recorder does, where to look, and known limits. A new scenario gets its
own section with the same parts.

## Audio clock drift (LiveKit microphones)

### What goes wrong

Some senders lose captured audio in bursts, before they encode it. Their RTP
timestamps count only the samples they kept, so their media clock runs slower
than the wall clock. A microphone recording timed by those timestamps alone is
shorter than the time it covered, and plays ahead of the video by the
difference.

### What the recorder does

The recorder measures each microphone's media clock against the packets'
arrival and against the sender reports LiveKit forwards. With
`livekit.audioClockSync: true` (the default), it moves the timestamps
forward by the media time lost. Each time the track falls 40 ms behind, it
inserts one gap, a multiple of 120 ms long. Playback fills a gap that long
with silence, without stretching the audio around it.

- A gap is inserted only when the sender reports show the loss, so a network
  delay alone does not add one, except in the cases under Known limits.
- When the publisher mutes and unmutes, LiveKit moves the downtrack's audio
  timestamps forward to the wall clock. The recorder then gives back the gaps
  that this jump already covers.
- Only LiveKit microphones are corrected. Screen-share audio and the mediasoup
  adapter keep the sender's timestamps.

### Where to look

- **Log:** `Audio media clock behind the wall clock: inserting timestamp gaps`
  at the first gap of a track, and `Audio timestamps jumped ahead of their
  arrival: LiveKit moved them to the wall clock` at its first realignment. The
  `Track recording stopped` line of a microphone carries the stats file's
  fields below, under the same names.
- **Drift the recording kept**, two estimates:
  - By the packets' arrival: `lagMs` at the end, after the gaps. At 200 ms or
    more, the log warns `Microphone ended behind the wall clock by its
    packets' arrival`. A network delay that grew near the end adds to it.
  - By the sender reports:
    `-reportedDriftMs - realignedMs - insertedGapMs + givenBackMs`. A network
    delay does not change it, but it misses media lost after the newest
    report.
- **Stats file** (`recorder.writeStatsFile`), under
  `captureStats.tracks.MICROPHONE.mediaClock`:
  - `rateRatio`: media time over arrival time, below 1 for a slow sender
    (`rtpDurationMs` less `realignedMs`, over `arrivalDurationMs`).
  - `reportedDriftMs`: the sender's media time less its wall time, by its
    sender reports; negative when slow, as LiveKit's `driftMs`.
  - `insertedGaps` and `insertedGapMs`: the gaps inserted.
  - `realignments` and `realignedMs`: how many times LiveKit moved the
    timestamps to the wall clock, and by how much. A realignment is a jump
    ahead of the arrivals that the next sender reports confirm. `rateRatio`
    and `reportedDriftMs` leave the realignments out, so they show the
    sender's own clock.
  - `givenBackMs`: the gaps given back where the timestamps jumped ahead of
    their arrival.
  - `lagMs` and `maxLagMs`: how far behind the wall clock the track is after
    its gaps, positive when behind. They come from the packets' arrival, so a
    network delay that grows adds to them.
  - `senderReports`: the sender reports used. `senderReportsIgnored`: those
    repeated or outside the stream's timestamps. `senderClockSteps`: those
    whose NTP or RTP time stepped; they add no loss.
- **Metrics:** `recorder_media_clock_rate_ratio` and
  `recorder_media_clock_end_lag_seconds` (what was left at the end, by the
  packets' arrival) observe each track of 10 s or more.
  `recorder_media_clock_inserted_gaps_total` and
  `recorder_media_clock_inserted_gap_seconds_total` count the gaps of every
  track.
- **Trace capture** (`livekit.traceCapture`): the sender reports appear as
  `sr` records next to the packets. The packets keep the sender's own
  timestamps, without the inserted gaps.
- **livekit-server:** `received sender report, clock skew against media path`
  names the publisher once its drift passes 5 s. Its `packetDrift.clockRate`
  over the codec rate is the rate ratio, and `ntpReportDrift.driftMs` is the
  drift. Both count from the publication's start, not the recording's.
- **Code and tests:** `MediaClock` (`internal/webrtc/utils/mediaclock.go`)
  holds the correction, and the LiveKit read loop applies it before the
  jitter buffer. The replay corpus seeds `c18-slow-mic-clock` and
  `c18-delay-step` replay a slow microphone and a healthy one behind a delay
  step. `c18-mic-clock-gap-at-start` replays a correction in a microphone's
  first 2 s, while its start can still move. `TestSlowMicClockKeepsWallClock`
  (`tests/e2e`) covers a slow microphone end to end.

### Known limits

- **Delay before a correction.** A burst is corrected only once a sender
  report shows it. Until then, the audio plays early by the media lost since
  the previous report: up to about 300 ms at 4.5 %, with Chrome's reports
  about 5 s apart and LiveKit forwarding them every 3 s. The loss before the
  first report waits longer: it is corrected only once the reports span 5 s
  and show its rate. If they show no loss in their first 30 s, it is never
  corrected. With Chrome losing 4.5 %, the start of a recording played up to
  0.7 s early.
- **Network delay rising after a burst.** For up to 11 s after the sender
  reports last show the loss growing, the recorder still allows gaps at about
  the burst's rate. A network delay that rises in that window adds gaps that
  stay. The extra gaps total at most the burst's rate times 11 s: about 0.5 s
  at 4.5 %. In tests, a 400 ms rise added up to 0.36 s at 4.5 % and 0.24 s at
  2 %, and a rise 12 s after the burst ended still added one or two gaps.
- **Noisy sender reports.** Reports whose RTP times are off by 15 ms or more,
  with a network delay rise of 150 ms, can give a healthy sender one 120 ms
  gap. It did not happen at 10 ms or less, nor with Chrome in tests.
- **Timestamps that jump back.** After a backward jump of more than 10 s, a
  late packet from before the jump reads as a forward jump and gives back all
  the gaps inserted so far. LiveKit keeps a downtrack's audio timestamps from
  going back, and the WebM writer skips any frame that would.
- **A sender whose clock runs fast.** It is only measured. Correcting it would
  mean dropping frames.
- **Realignment counts.** `realignments` and `realignedMs` can be off, and with
  them `rateRatio` and `reportedDriftMs`; no correction reads them. A
  realignment goes uncounted before the first sender report, after the last,
  or when the sender loses at least three quarters as much in the same report
  interval. With a sender clock 2 % fast, a network delay that drops during
  silence can count as a realignment: up to 400 ms with reports 5 s apart.

## H.264 cameras (LiveKit)

### What goes wrong

An H.264 frame usually spans several RTP packets: one NAL unit split into
FU-A fragments, or several NAL units packed into one STAP-A. A frame missing
any packet cannot be decoded, and neither can the frames after it until the
next IDR. A key frame also needs its own SPS and PPS: an IDR decoded with
older parameter sets shows garbage after a resolution change.

LiveKit adds cases of its own:

- **Simulcast layer switch.** The old layer's last frame can be cut short,
  and arrive in the same jitter-buffer sample as the new layer's key frame.
  An SPS of one layer can also arrive without its IDR.
- **Blank frames.** LiveKit can send 2x2 key frames, for example when a
  downtrack closes. They are valid H.264, but would set a 2x2 track size.
- **Padding.** LiveKit 1.13.5-1.13.9 sends padding with a 255-byte payload,
  which looks like media unless the padding bit is checked.
- **Backup codec.** A publisher can list VP8 as a backup for H.264. LiveKit
  may send either one, and can switch to the other mid-session.

### What the recorder does

- **Whole frames.** The track buffer releases H.264 as whole samples, not
  packets. A sample is marked `follows` when no packet was lost since the
  previous one, and every packet between them was padding or empty. A sample
  that does not follow, or a frame broken inside a sample, stops writing
  until the next key frame. The recorder then requests one, at most once a
  second, and not while one is due.
- **Broken frames inside a sample.** Parsing goes on past a broken frame, so
  a key frame later in the same sample, as after a layer switch, is written.
- **Key frames.** A key frame is an IDR with an SPS and a PPS in the same
  frame, from 16x16 to 8192x8192, whose SPS parses. Blank frames and an IDR
  whose parameter sets were lost are not key frames.
- **File and track header.** The file is Matroska (`.mkv`) when a requested
  publication lists H.264, primary or backup. The track's size and
  `CodecPrivate` come from the first key frame's own SPS and PPS. Every key
  frame keeps its SPS and PPS in band, so later sizes still decode.
- **Codec changes.** At a codec regression, LiveKit closes the recorder's
  subscription and subscribes the track again in the new codec. The first
  reader's end stops the session as `closed`; the new subscription is not
  recorded. The README's protocol section says what a requester does next.
- **Timestamps.** The presentation time counts the RTP ticks since the first
  key frame. A delta frame whose timestamp goes back is skipped, and so are
  the frames after it until a key frame. A key frame behind the last frame
  written starts a new timestamp base. It is placed after that frame by the
  time between their arrivals, and at least 1 ms after it. A jump of 2^31 to
  2^32 ticks (6.6 to 13.3 h), such as a long mute, reads as a step back.

### Where to look

- **Log:** `First video/h264 key frame: timestamp=..., WxH` once per
  recording. `Discarding malformed video/h264 frame` names the broken packets
  and why, and `Discarding invalid video/h264 frame` a refused key frame.
  `Discarding video/h264 frame: ...`, at debug level, names the others.
- **Warnings:** those two, `Skipping video frame whose timestamp goes back`,
  `Video timestamps restart at a key frame`, `Duplicate frame detected` and
  `Discarding video/h264 frame of N bytes: the limit is M` are logged at warn
  level once per kind per recording. Later ones are at debug level.
- **Codec changes:** at a regression, the reader logs `video/H264 track=...
  stopped` and LiveKit logs `renegotiate new codec`. `Not recording track ...
  subscribed again with ...` marks a new subscription in another codec, and
  `Track ... changed codec from ... to ...` a change in place.
- **Stats file:** the camera's `mimeType` names the codec written,
  `keyframeCount` the key frames seen and `pliRequests` the key-frame
  requests sent. For H.264, `totalSamples` counts every frame, written or
  not, except broken frames and IDRs that are not key frames. H.264 sample
  durations are RTP intervals. With VP8's default assembler, they are the
  time each frame took to arrive.
- **Metrics:** the `mime` label is the publication's MIME type, also for a
  camera that LiveKit sends in its backup codec. On `recorder_active_tracks`
  it is the codec LiveKit sends.
- **Replay corpus:** the `h264-*` seeds in
  `internal/webrtc/livekit/testdata/corpus/README.md`. `h264-layer-switch`,
  `h264-simulcast-orphan-sps`, `h264-blank-close`,
  `h264-blank-close-midframe`, `h264-lost-stapa`, `h264-padding-payload` and
  `h264-padding-empty` cover the cases above. The `h264-c*` seeds and
  `h264-wrap-seq-ts` are twins of VP8 seeds, which that README's Source
  column names.
- **E2E:** `TestH264UnrepairedLossRequestsKeyframe`, `TestH264ClipDecodes`
  (an x264 clip decoded by ffmpeg) and `TestBrowserH264Camera` in
  `tests/e2e`.
- **E2E by case:** `TestH264SimulcastMediaTimestampsPlaceEachFrame` covers
  layer switches between frames and `TestH264UnpublishWritesNoBlankFrames`
  blank frames.
  `TestBrowserVP8CameraWithH264Backup` records a VP8 camera with an H.264
  backup.
- **Code:** `h264.go`, `h264_sps.go` and `frames.go` in
  `internal/webrtc/recorder`, and `trackbuffer.go` in
  `internal/webrtc/livekit`.

### Known limits

- **File name.** An H.264 recording ends in `.mkv`, whatever name the
  requester sent. A requester that writes the `.webm` name it sent into the
  recording events leaves the camera out of the published recording. The
  `.mkv` stays in the raw archive.
- **Start time.** After a restart of its own, bbb-webrtc-sfu takes each
  file's start time from a name ending in `.webm`. An `.mkv` name gives it
  no start time, so it can stop the file being recorded.
- **Parameter sets.** An IDR without its own SPS and PPS is not a key frame,
  and writing waits for an IDR with them. LiveKit itself does not start
  forwarding a sender whose key frames lack an SPS.
- **Track size.** The track header keeps the first key frame's size, which
  is often a reduced one: Chrome can start a 1280x720 camera at 320x180. A
  player that reads only the header reports that size.
- **Backup codec.** `startRecordingResponse` names the primary codec. When
  LiveKit sends the backup codec, the stats file, `recorder_active_tracks`
  and the log name it.
- **Packetization.** Modes 0 and 1 are recorded. Mode 2 (STAP-B, MTAP, FU-B)
  is refused as malformed; WebRTC senders do not use it.
- **Frame reordering.** The recorder writes frames in the order they were sent.
  Browsers never send reordered frames: libwebrtc drops a frame whose capture
  time does not increase. A non-browser publisher that sends B-frames with
  timestamps that go back gets only the frames from each key frame to the
  first one that goes back. B-frames with rising timestamps are written in
  decode order with those timestamps.
- **VP8 timestamps that go back.** The VP8 writer reads a timestamp step
  back as a jump of about 13 h forward. The rest of the file then starts 13 h
  later. Browsers do not step back; other publishers can.
- **mediasoup.** Only the LiveKit adapter records H.264.
