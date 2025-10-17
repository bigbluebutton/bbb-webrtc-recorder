# Changelog

All notable changes to this project will be documented in this file.

### UNRELEASED

* fix: panic when stopping a mediasoup adapter recording
* fix: unnecessary init of LK adapter fields when using mediasoup
* build(docker): bump golang from 1.24 to 1.25

### v0.13.0

* feat: add getRecordings API
* feat: add support for opaque session metadata
* feat: new Histogram metric `livekit_connect_duration_seconds`
* feat: new Histogram metric `livekit_subscribe_duration_seconds`

### v0.12.0

* feat: new Histogram metric `request_duration_seconds`
* feat: new counter metric `session_errors_total`
* fix: command processing is inefficient and causes lockups, rework it
* fix: don't emit stop event for sessions that have not started
* test: add double (trailing) start/stop tests to server.go
* test: regression test for stop events for non-started sessions

### v0.11.0

* feat(livekit): generation of rtpdump files for recorded tracks
* fix(mediasoup): stop event triggered by flowing=false if adapter is closed

### v0.10.0

* feat: boot sequence health check and Prometheus health metrics
* feat: report readiness via sd_notify
* build: upgrade to Pion v4 (fixes periodic freezes/tuple restarts)

### v0.9.4

* feat(livekit): add Prom counters for reconnects and sub failures
* fix(livekit): guarantee cleanup on start recording failures
* fix(livekit): fix deadlock with jitterbuffer usage
* fix(livekit): handle track subscription failures
* fix(livekit): use tokens for connecting to a room
* fix(livekit): handle track EOF errors
* fix(livekit): reject requests when no track is available
* fix(livekit): reject requests when not all tracks are subscribed

### v0.9.3

* fix(livekit): add panic recovery when pushing packets
* fix(livekit): copy audio pkts before pushing to samplebuilder
* build(deps): bump actions/setup-go from 4 to 5

### v0.9.2

* fix(livekit): prevent duplicate stop events

### v0.9.1

* fix(livekit): deadlock on GetStats call
* fix: inconsistent active_tracks metric

### v0.9.0

* feat: extend RPCs to support different recording adapters
* feat(livekit): initial support for recording LiveKit tracks (audio, video, screen)
* feat(livekit): implement RTP status change events
* feat: write capture stats to file
* feat(livekit): add writer-level stats
* feat(livekit): add extended Prometheus metrics for adapter/recorder stats
* feat: implement active_tracks Prom metric
* feat: implement in/out/invalid request metrics
* fix: failing jitterbuffer tests
* fix: startRecording did not reject unknown adapters
* fix: add graceful shutdown
* fix: retry Redis reconn if it drops
* fix(livekit): properly handle RTP read errors, add metrics for them
* fix: normalize webrtc.go to use receivers as pointers
* fix: errcheck and log config unmarshal failures
* build: remove ebml-go mirror, use upstream v0.17.1
* build: add basic golangci-lint workflow
* build: add a workflow for `go test`
* refactor: make log.level configurable, remove debug var

### v0.8.1

* build: Automatic build pipeline for docker images
* build: update Dockerfile to match bigbluebutton/docker's
* build: remove broken package workflow

### v0.8.0

* fix: multiple adjusments to VP8 sample building in the WebM recorder
* fix: multiple adjustments to packet loss handling
* fix: better pts generation for video samples
* fix: edge case adjusments to RTP jitter buffer and receive log
* chore: change default JB size
* feat: add option to write IVF copies of recorded streams
* feat: add option to use alternative video sample builder
* feat: make jb packet timeout configurable
* feat: make EBML max write queue sizes configurable
* build: use Galene's samplebuilder instead of Pion's
* build: sync ebml-go with upstream (4b8f657f0)

### v0.7.0

* fix: panic due to invalid OPUS samples pushed to builder
* build(docker): go 1.21
* build: bump pion/webrtc/v3 to v3.2.24

### v0.6.0

* feat: recorder.writeToDevNull option to write files to /dev/null (testing)
* fix: panic due to negative seqnums in sequence unwrapper

### v0.5.2

* fix: lock EBML write and close ops
  - Fixes a crash
* build(docker): separate build and run stages, add APP version arg

### v0.5.1

* fix: add onStart param to Subscribe call in http module

### v0.5.0

* feat: add getRecorderStatus/recorderStatus RPCs

### v0.4.1

* fix: change file mode of rec dir in nfpm scripts to 0700
* fix: change to working env prefix BBBRECORDER_, add docs on env vars
* fix: split dir and file modes, make the configs string

### v0.4.0

* feat: add recorder.fileMode config

### v0.3.1

* fix: create media files with perm 0700
* fix: only generate audio tracks if peer has audio

### v0.3.0

* feat: add basic Prometheus instrumentation
* fix: CPU lock when reading packets from JB
* build: add basic Dockerfile

### v0.2.0

* First release
