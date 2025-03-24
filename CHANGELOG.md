# Changelog

All notable changes to this project will be documented in this file.

### UNRELEASED

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
