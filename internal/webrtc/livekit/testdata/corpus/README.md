# LiveKit replay seed corpus

Read traces for the in-process replay of the LiveKit path (read loop, `trackBuffer`, `WebmRecorder`), one `<name>.expect.json` per seed.

- **Traces** are `internal/corpus` trace v1: gzipped NDJSON, a header line and one record per packet read, ending with an `eof` record. `atUs` is the read time.
  - **Event records** log what the reader did: `pli`, a keyframe request with its `reason` (`packet_drop` or `direct_request`), and `timeout`, a read that hit its deadline. Replay skips them, since the events re-emerge from the replayed packets and read times.
  - **The `eof` reason** is optional. A capture sets it when its trace misses records: `truncated` when it was cut at its byte cap, `dropped` when its writer fell behind and dropped records.
  - **`pad`** marks an RTP padding packet. LiveKit sends padding probes ahead of a track's first key frame, and the jitter buffer drops them before it starts.
  - **`epochUnixUs`** in the header is the wall-clock time of `atUs` 0. Each recording session has its own clock, so the traces of one meeting align only through it.
- **No trace carries media.** Every payload is a header prefix that replay zero-pads to `plen`:
  - a VP8 descriptor and frame header in the `corpus.GenerateVP8` layout;
  - an Opus TOC byte.
- **Provenance:**
  - `synthetic`: built with the `corpus` generators and mutations. The header's `generator` field records the exact call chain.
  - `pcap-extract`: per-packet read schedules taken from header-only SRTP captures of live test runs. The header `note` says what was synthesized.
  - `capture`: a live capture written by the LiveKit adapter with `livekit.traceCapture` enabled. Its `generator` names the payload mode; in `full` mode the trace carries the media. No seed uses it.
- **Regenerating:** the converters that wrote these traces are not in this repository, only their output is. The expect files are calibrated by hand from measured runs.

## Seeds

The class groups the seeds that guard one defect, named in the Guards column. A RED on a class-9 seed, for example, means the push-lag window defect has come back. Each seed's expect file states its RED in `intent`.

| Seed | Class | Provenance | Source | Guards |
|---|---|---|---|---|
| `c01-paced-6x45` | 1 | synthetic | a 3.4 fps camera pacing each frame over 225 ms | Paced key frames dropped by a short expiry window |
| `c01-trickle-11x50` | 1 | synthetic | frames trickled over 500 ms, back to back | Same defect, with a 500 ms trickle per frame |
| `c01-px-ml-ab-1-reccam` | 1 | pcap-extract | the recorder's camera downtrack in a live run with mixed join and leave | The live keyframe loop in mixed-leave: 3.5 fps, frames spread up to 497 ms |
| `c02-opus-tail-blank` | 2 | synthetic | `webm_audio_test.go` | LiveKit's close-out blank pair, with one frame +1 tick past the last |
| `c03-d11-30fps` | 3 | synthetic | `TestTrackBufferLowFrameRateRetransmit` bursts | An unrepaired mid-frame loss is requested only at the 2 s expiry |
| `c03-d11-5fps` | 3 | synthetic | same | Same, at 5 fps |
| `c03-d11-3.4fps` | 3 | synthetic | same | Same, at 3.4 fps (the fake-device camera rate) |
| `c05-repaired-5fps` | 5 | synthetic | `TestTrackBufferLowFrameRateRetransmit` in-band and RTX cases | A repaired loss at 5 fps requests a key frame |
| `c09-mx-rev5b-2-drop1` | 9 | pcap-extract | a slice of a live camera track, one packet withheld | Push-lag window: the writer asks again when frames reach it 20 ms past the window |
| `c09-mx-rev5-1-residual` | 9 | pcap-extract | a live camera track, seq 13100 withheld | Of every packet withheld in turn while the stream flows, the one whose frames reach the writer latest: an accepted residual, pinned rather than 0 |
| `c10-two-losses-1500ms` | 10 | synthetic | generators | Two losses 1.5 s apart, beyond the 1 s loss-request limit |
| `c11-dups-30fps` | 11 | synthetic | a duplicate inside a frame still assembling, one after its frame left | Duplicates underflow `PacketsLost` |
| `c11-dup-held-700pps` | 11 | synthetic | `TestTrackBufferDropsDuplicateOfHeldPacketAtHighRate` | A duplicate of a held packet, 1050 seqs behind, at 700 pkt/s |
| `c12-lkprobe-rtxdup` | 12 | synthetic | LiveKit's RTX padding probe, at a frame boundary and mid-frame | A probe resends the last 6 packets over RTX |
| `c15-start-midframe` | 15 | synthetic | generators | A subscription that starts inside the first key frame |
| `c15-start-kfloss` | 15 | synthetic | generators | The first key frame loses a packet |
| `c17-eof-after-gap` | 17 | synthetic | generators | Eof right after a gap |
| `wrap-seq-ts-pic` | wrap | synthetic | generators | Wraps of seq, timestamp and picture ID, with a repair at seq 0 |
| `wrap-unrepaired-loss` | wrap | synthetic | `c03-d11-30fps` moved onto the seq wrap | An unrepaired mid-frame loss at seq 65535, revealed by seq 0: a gap tracker that takes the wrap for a jump back loses the gap |
| `wrap-dup-probe` | wrap | synthetic | `c12-lkprobe-rtxdup` moved onto the seq wrap | RTX probe copies of pre-wrap packets read after the wrap: a duplicate window that takes the wrap for a jump back lets them through |
| `baseline-av` (`.video` + `.audio`) | baseline | synthetic | generators | The ordinary path |

## Expect v1

`TestCorpusReplay` (`corpus_replay_test.go`) replays every expect file, and fails if a trace is not listed by exactly one. Unknown fields fail the load. Every check is optional: `bufferRequests`, `bufferRequestsMs`, `writerRequests`, `firstRequestMinMs`, `firstRequestMaxMs`, `written`, `packetsWritten`, `packetsLostMax` and `webm`. The required fields are:

- `v`: 1;
- `class`;
- `intent`: what the seed checks and its RED;
- `traces`: the files replayed together into one WebM.

**Conventions:**
- **Requests** are counted where they are made:
  - `bufferRequests` at the track buffer's `onLoss`;
  - `writerRequests` at the writer's own requester.
- **`firstRequest*Ms`** are inclusive, in ms on the trace's `atUs` timeline, over requests of both origins.
- **`bufferRequestsMs`** gives one inclusive window per track-buffer request, in order; the count must match.
- **`written`** counts the video plus audio frames the writer wrote.
- **`packetsWritten`** counts the packets the track buffers released to the writer. Where set, it is pinned exactly.
- **`packetsLostMax`** bounds the jitter buffers' `PacketsLost`, summed over the tracks.
- **`webm`** checks the file read back with `oracle.SummarizeWebM`. Key frames are counted by the VP8 payload bit. A missing file fails every `webm` check: nothing written leaves none.
- **Every seed:** the file must hold as many blocks per track as the writer counted frames, so a frame the writer counts but loses fails without a check of its own.
- **`residuals`** is a list of notes, one per check pinned away from its ideal value: what the ideal is, and why the pin differs.

## Determinism

The replay goes through the real `readTrack` with a fake track source, inside a `testing/synctest` bubble. A jitter buffer timer that fires at the instant of a read runs before it, and 20 runs (`-count=20`) give identical results for every seed.

## Closed loop

The keyframe loop needs a publisher that answers requests, which a fixed trace cannot do. `TestKeyframeLoopDoesNotPersist` models it in Go:
- a camera at F fps, running for 20 s, with one frame about 3 s in missing a packet that is never repaired;
- each request that reaches the publisher is answered 120 ms later with a key frame of 16 packets;
- a FIFO pacer spreads that key frame over S ms, within the 480-664 ms of the live mixed flow.

| Case | 2 s (production) | 200 ms |
|---|---|---|
| 3.4 fps, 6-packet deltas, S = 664 ms, frame 10 lost | 1 buffer request, 588 ms after the loss; 1 key frame; 65/68 frames | 17 requests up to 19.6 s; 16 key frames; 10 frames |
| 15 fps, 3-packet deltas, S = 480 ms, frame 45 lost | 1 buffer request, 200 ms after the loss; 1 key frame; 295/300 frames | 13 + 13 requests up to 19.9 s; 25 key frames; 45 frames |

The test requires exactly 1 request, within `lossKeyframeInterval` of the loss, and 1 answering key frame. It also requires at least 65 or 295 frames written. A loop that persists asks again every `lossKeyframeInterval`, so it fails the count. It also writes nothing after the loss, so it fails the frame bound as well.

## pcap-extract details

These traces carry header fields and capture times only:
- **Times:** capture time is used as read time.
- **Payload:** S is set on the lowest seq of each RTP timestamp, and every frame gets a picture ID.
- **Key frames:** the first frame of the trace, and every frame of 10 or more packets.
  - The captures' delta frames have 6-8 packets and their key frames 11-21.
  - The rule matches the frame list a subscriber decoded from the same runs.
- **Blank frames:** LiveKit's close-out blank frames are synthesized as 8x8 key frames.
- **`plen`:** estimated as the wire length minus the RTP header and a 16-byte SRTP tag.
- **Padding:** padding-only probes are recorded with `plen` 0.
