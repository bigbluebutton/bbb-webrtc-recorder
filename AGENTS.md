# AGENTS.md

How to build, test and verify changes to bbb-webrtc-recorder, and the pitfalls the code does not show. `CLAUDE.md` is a symlink to this file.

## Project Overview

bbb-webrtc-recorder is a Go service for BigBlueButton that records WebRTC media streams (audio/video) to WebM files. It supports two WebRTC adapters: **mediasoup** (direct pion/webrtc peer connection) and **LiveKit** (via LiveKit server-sdk-go). The service communicates via Redis pub/sub for session control (start/stop recording, status queries).

## Build & Run

`make` lists the targets. The Makefile is the entry point; the commands behind each target are in it.

```bash
make build    # build/bbb-webrtc-recorder, version from ./VERSION
make run      # run with config/bbb-webrtc-recorder.yml (CONFIG= overrides); needs Redis

# Dev build with debug symbols
go build -o bbb-webrtc-recorder -gcflags=all="-N -l" ./cmd/bbb-webrtc-recorder

# Dump config values
./build/bbb-webrtc-recorder [-c config.yml] --dump recorder.directory
```

## Testing

```bash
# Unit tests with -race, plus the vendored media-sdk jitter copy, which is its own
# module (go.mod `replace`) that ./... skips. CI runs the same, so a race fails the job.
make test

# A single test
go test -v -run TestName ./internal/path/to/package/

# CI (.github/workflows/test.yml) sets BBBRECORDER_* (log level, recorder
# directory and modes, pubsub adapter, Redis address) to mirror a runtime config.
# No test reads them: tests build config.Config directly and write to temp dirs.
```

Write new tests with testify (`assert`, `require`), not `testing.T.Error/Fatal/Fail`; lint (`forbidigo`) flags those in new code. Older tests (appstats, pubsub/events, server, webrtc/utils) still use raw `testing.T` calls.

- LiveKit adapter unit tests: `setupMockLK()` (`livekit_test.go`) builds an adapter without a room; an empty `&lksdk.RemoteTrackPublication{}` is enough for `GetStats` (lksdk has no public setter for TrackInfo).
- E2E suite (`tests/e2e/`, `e2e` build tag): the recorder binary against its own `livekit-server` and `redis-server` on loopback, no Docker. `tests/e2e/README.md` has "Adding a scenario".
  - Run: `LIVEKIT_SERVER_BIN=/usr/bin/livekit-server make test-e2e`
  - `TestNACKRepairUnderLoss` needs root and `E2E_NETEM=1`; without sudo, run it inside `unshare --user --map-root-user --net --mount` with a tmpfs over `/run/netns` and `ip link set lo up`.
  - CI (`.github/workflows/e2e.yml`): PRs touching `internal/`, `cmd/`, `third_party/`, `tests/e2e/`, `go.mod`/`go.sum`, or labelled `e2e`.
  - `E2E_RECORDER_RACE=1` builds the recorder with `-race`; a race report in its log fails the test. CI runs the e2e suite with and without it.
  - Browser mode: `TestBrowser*` scenarios publish from Chromium, which Playwright drives through the Node runner in `tests/e2e/browser`. They skip unless `E2E_BROWSER=1`; setup is in the README's "Browser mode".
- In-process replay corpus: `internal/webrtc/livekit/testdata/corpus/README.md` covers adding a seed. `go run ./internal/corpus/seedgen` regenerates the traces (`TestCorpusMatchesGenerator` guards drift); `go test ./internal/webrtc/livekit/ -run 'TestReplayTraces$' -v -args -corpus.replay=<abs trace>` replays a capture with no expect file. The timed livekit tests run under `testing/synctest`.

## Linting

```bash
make lint       # issues new since the merge base with develop (LINT_BASE= overrides)
make lint-all   # the whole tree, including issues that predate a change
make format     # gofmt and goimports, per .golangci.yml
```

golangci-lint runs from `tools/go.mod` through `go tool`, so no global install is needed and its dependencies stay out of the main module. CI (`golangci-lint-action`, `only-new-issues`, `--build-tags=e2e`) pins the same version in `.github/workflows/golangci-lint.yml`; bump both together.

`.golangci.yml` is a baseline: v2's standard linters (errcheck, govet, ineffassign, staticcheck, unused), `forbidigo` for the testify rule, and the gofmt and goimports formatters. The tree still has issues that predate it, which is why lint reports only new ones. Stricter linters are added one at a time, each with its cleanup.

**Conventions lint does not enforce yet:** outside `cmd/`, no `fmt.Print*`, stdlib `log.Print*/Fatal*/Panic*`, `os.Exit`, `panic` or `print/println`; use `logrus` (imported as `log`) for all logging. Use `any` instead of `interface{}`.

## Architecture

### Request Flow

1. **PubSub** (`internal/pubsub/`) - Redis adapter receives JSON messages on a subscribe channel
2. **Server** (`internal/server/server.go`) - Decodes events, dispatches by `id` field (`startRecording`, `stopRecording`, `getRecorderStatus`, `getRecordings`)
3. **Session** (`internal/server/session.go`) - Each recording session runs in its own goroutine with a command channel. Sessions manage lifecycle, callbacks, and coordinate between the adapter and recorder
4. **WebRTC Adapters** - Two paths:
   - **Mediasoup** (`internal/webrtc/webrtc.go`) - Direct pion/webrtc peer connection with SDP offer/answer, NACK handling, and PLI requests
   - **LiveKit** (`internal/webrtc/livekit/livekit.go`) - Connects to LiveKit room via server-sdk-go, subscribes to specific tracks by ID (track mute/unmute is only logged)
5. **Recorder** (`internal/webrtc/recorder/`) - `Recorder` interface with `WebmRecorder` implementation. Receives RTP packets via `PushVideo`/`PushAudio`, muxes VP8+Opus into WebM using ebml-go

### Key Components

- **`internal/app/`** - Application bootstrap (init, signal handling, config loading). Uses Go `init()` pattern - importing the package starts the app
- **`internal/config/`** - Config struct definitions and loader. Supports YAML files + env vars prefixed with `BBBRECORDER_` (nested with `_`)
- **`internal/pubsub/events/`** - Event types for the pub/sub protocol (see README for full JSON5 schemas)
- **`internal/webrtc/utils/`** - Jitter buffer, send buffer, NACK pair generation, receive log, sequence unwrapper, connection state normalization
- **`packaging/`** - nfpm deb (`nfpm.yml`, systemd unit, env file); ships `config/bbb-webrtc-recorder.yml` as `/etc/bbb-webrtc-recorder/bbb-webrtc-recorder.yml` (also copied by the Dockerfile), so new config defaults go in `SetDefaults()` (`internal/config/config.go`) and are mirrored in that file
- **`internal/webrtc/recorder/webm.go`** - WebM muxer: VP8 frames through a custom assembler (`recorder.useCustomSampler`, default on) or jech/samplebuilder, Opus through jech/samplebuilder
- **`internal/webrtc/utils/mediaclock.go`** - per-track `MediaClock`: moves a LiveKit microphone's RTP timestamps forward by the media time its sender loses, by arrival times capped by the sender reports (`livekit.audioClockSync`), so the WebM keeps the wall clock. The read loop applies it before the jitter buffer; trace and RTP dump keep the sender's timestamps
- **`internal/webrtc/livekit/trackbuffer.go`** - per-track jitter buffer around media-sdk's `jitter`: early keyframe request when a gap stays open, duplicate filtering, padding handling. `capture.go` is the opt-in `livekit.traceCapture`
- **`third_party/media-sdk/`** - vendored media-sdk `jitter` with local fixes (`go.mod` `replace`; changes listed in `third_party/README.md`)
- **`internal/corpus/`** - trace format, generators and mutations, WebM oracle (`oracle/`), and `seedgen` for the replay corpus
- **`internal/appstats/`** - Prometheus metrics and per-session stats file writing
- **`internal/webrtc/interfaces/`** - Interface definitions used as seams and for mocking (e.g. KeyframeRequester, LiveKitWebRTCInterface)
- **`web/`** - Embedded filesystem for HTTP test UI
- **`internal/diskpressure/`** - statfs monitor for the opt-in disk-pressure guard (`constraints.diskPressure`); every check is timed out
- **`internal/server/constraints.go`**, **`internal/webrtc/livekit/constraints.go`** - capture quality constraints: a server-side reconciler fans changes out to sessions; each LiveKit capture keeps a `constraintBook` (mediasoup sessions ignore constraints)

### Config Loading Order

1. Defaults set in `config.SetDefaults()`
2. ONE YAML file, first found wins (no merging): `-c <file>` or `./bbb-webrtc-recorder.yml`, then `/etc/bigbluebutton/bbb-webrtc-recorder.{yaml,yml}`, `/etc/bbb-webrtc-recorder/bbb-webrtc-recorder.{yaml,yml}` (packaged), `$HOME/.config/bbb-webrtc-recorder.{yaml,yml}`, `./bbb-webrtc-recorder.{yaml,yml}`. An override file under `/etc/bigbluebutton/` replaces the packaged file entirely.
3. Environment variables with `BBBRECORDER_` prefix override file config

### Supported Codecs

VP8 (video) and Opus (audio) only. Output format is WebM (with optional IVF copy for video).

## Key Conventions

- Logging: use `logrus` (`log` alias) with structured fields, especially `session` field for per-session context
- Config env vars: `BBBRECORDER_` prefix, uppercase, underscore-separated (e.g., `BBBRECORDER_RECORDER_DIRECTORY`)
- Version is in `./VERSION` file and injected via `-ldflags` at build time through `internal.AppVersion`
- Go version: the `go` directive in go.mod (1.26)

## Gotchas

- **Empty `main.go`**: The app bootstraps entirely via `init()` in `internal/app/app.go`, triggered by a side-effect import (`import _ "...internal/app"`). All startup, config loading, and the main loop happen in `init()`. Do not add logic to `main()`.
- **Docker build**: multi-stage `Dockerfile`, a `golang` builder (version in its `FROM`) and a `debian:bookworm-slim` runtime. Build with `docker build -t bbb-webrtc-recorder .`
- **LiveKit adapter lock order**: `Server.constraintsMu` → `LiveKitWebRTC.constraintMu` → `m`, and `WebmRecorder.m` → `m`. The recorder requests key frames from the adapter with its lock held, so the adapter calls the recorder with `m` released. Never hold `m` together with a jitter buffer's lock: buffers call their drop or loss handler under their own lock (with the media-sdk jitter buffer, also from its timer goroutine), so the handler only queues keyframe requests, and `GetStats` reads buffer stats after releasing `m` (guarded by `jitter_lock_test.go`). Where trace capture exists (`livekit.traceCapture`), the handler also records each request under `capturesMu`, which is kept apart from `m` for the same reason (`TestJitterLossHandlerWithCaptureDoesNotTakeAdapterLock`). No lksdk call that writes to the signal connection (e.g. `SetVideoQuality`, sent under `constraintMu` only) under `m`.
- **LiveKit 1.13.5-1.13.9 padding** reaches the recorder with `Padding` set and a 255-byte payload (`PaddingSize` 255 on top of it); 1.13.10 (#4993) sends it with none again. Identify it by `packet.Padding`, not `len(Payload) == 0`, in code and in test models, and reproduce padding bugs on the e2e job's pinned 1.13.7, not the host's `livekit-server`.
- **CI lint `typecheck` errors** such as "export data version 5 is greater than maximum supported version 2" mean the Go from go.mod (`setup-go` uses `go-version-file: go.mod`) writes export data newer than the pinned golangci-lint can read. Bump golangci-lint (`tools/go.mod` and the workflow's `version:`) together with Go; it is not a code error.

## Commit conventions

Conventional-commit title, lines capped at 72 characters, imperative mood, present tense for the status quo
("The action triggers a mute request", not "triggered a mute request"). Separate commits for logically separate
changes.

The body answers **what is wrong, why this change is better, and how it works** — and nothing else.
These three "answers" should be in separate textual blocks for easier reading.
Each block may be composed of multiple paragraphs, and no specific separator is required
between blocks - the idea is for the log to be readable as a decently readable narrative
for humans as well.

Be succinct; a body too long usually means the commit should be split or that
the log should be reworded or summarized further.

Write commit bodies and comments as described in "Writing style" below.

Leave out anything transient that a reader of the final tree cannot identify:
- how the defect was discovered ("found while auditing X", "reported from a deployment");
- What the code looked like mid-session
  * If an alternative implementation was considered, then discarded, this can be
    part of the commit log - but very briefly, and only if it helps the reader understand the final choice.
- tables or enumerations a reader can produce with one grep;
- restating what the diff plainly shows.

## Inline comments

Comment what is **not obvious from the code**, and say *why*, not *what*. A declaration that looks arbitrary or
wrong until you know the constraint deserves a comment; the rest does not.

Same exclusions as commit bodies: no incident narrative, no "this used to be", no explanation of a state that no
longer exists in the tree. Do not state the same fact in two comments.

## Writing style

Commit messages and comments are read once, by someone new to the code. Write for that reader:
- Plain words. Use the code's own names and common Go/WebRTC terms; do not coin phrases
  ("unrepaired loss", "the bound", "hands the frames to", "a subscription lands").
- Short sentences, one idea each. Say what happens, then why it matters.
- A sentence that needs a second read gets rewritten, even if it is accurate.

Example, a comment on a timeout:

```
Bad:  Readers end when lksdk closes the subscriber peer connection, which Disconnect starts
      in the background; a read deadline does not end them. A stop during a signal recovery
      waits the whole bound, since lksdk holds that close until the recovery ends.
Good: Track readers end when lksdk closes the subscriber peer connection; the read deadline
      alone does not end them. Close can take much longer while, e.g., the peer is
      reconnecting. This timeout limits that wait.
```
