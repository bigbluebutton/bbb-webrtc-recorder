# Changelog

All notable changes to this project will be documented in this file.

### v0.6.0 (UNRELEASED)

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
