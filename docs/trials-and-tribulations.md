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
`livekit.audioClockSync: true` (off by default), it moves the timestamps
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
  step.

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
