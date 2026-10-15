# Recorder end-to-end tests

These tests run the recorder binary against a real `livekit-server` and
`redis-server` on loopback. They drive the recorder over Redis the way
bbb-webrtc-sfu does, and publish generated media from a Go LiveKit
participant (lksdk) running inside the test process. Nothing runs in Docker.
Every process gets its own free loopback ports, so the suite never touches
LiveKit, Redis or recorder instances already running on the host. A child
that loses a port to another process before binding it is started once more
on fresh ones. LiveKit gathers candidates on `lo` only, and hands clients a
STUN server on a loopback port nothing listens on, so no traffic leaves the
host.

The files carry the `e2e` build tag, so a plain `go test ./...` skips them.

## What runs

For each test, the harness:
- starts `livekit-server` from a generated loopback-only config using the
  well-known dev key pair (`devkey`/`secret`), and a `redis-server`;
- starts the recorder, built once per run from `./cmd/bbb-webrtc-recorder`.
  Its generated config turns on Prometheus, trace capture (header mode) and
  the stats file, and gives it its own recordings directory;
- publishes VP8 and Opus tracks from `internal/corpus` generator schedules
  in real time. The publisher answers each PLI with a key frame after a set
  delay, paced over a set spread, and can withhold packets (loss nothing
  repairs) or pause whole frames.

Each test checks the WebM through `internal/corpus/oracle`, along with the
recorder's trace, the `recordingStopped` events and the `/metrics` deltas.

| Test | Scenario |
|---|---|
| `TestBaselineRecording` | **Baseline.** A camera and a microphone, one session per track, about 10 s on a clean path. It checks the frame counts against what was sent and the PTS (increasing, no duplicates, gaps bounded), that the trace and the metrics agree, and that the recorder asks for no key frames. |
| `TestUnrepairedLossRequestsKeyframe` | **Unrepaired loss.** One packet is withheld from the middle of a camera frame, at 15 and at 5 fps. The trace must show exactly one unrepaired gap, at that packet. The recorder must request a key frame early, from the sequence gap (trace PLI reason `packet_drop`), within a bound that depends on the fps, and frames must be written again after the publisher answers. |
| `TestPacedKeyframesNoLoop` | **Paced key frames.** A 3.4 fps camera whose key frames are paced over 664 ms, plus one lost packet. Key-frame requests must stay bounded (no keyframe loop) and frames must keep being written. |
| `TestFlowingFollowsFirstMedia` | **Flowing at start.** A camera answers the recorder's key-frame request 400 ms late, so LiveKit sends padding ahead of the first frame. The first `flowing` event, on which the SFU stamps the recording's start, must not reach the driver before the recorder reads the first media packet, and the trace must hold padding before that read. Only LiveKit 1.13.5 to 1.13.9 give that padding a payload, so only they fail it on a recorder that takes a payload for media. |
| `TestAudioAfterTrailingLossIsWritten` | **Audio after a trailing loss.** One microphone packet is withheld shortly before the recording stops, in two cases. In the first the source pauses 10 frames later; in the second it keeps talking and the stop comes 3 frames later, within the adapter's 200 ms jitter latency. Both times the recorder has not moved past the loss when it stops. Every frame the recorder read after the loss must be in the WebM. The scenario checks that it read some, and fewer than 32. |
| `TestNACKRepairUnderLoss` | **NACK/RTX repair under loss.** `livekit-server` and `redis-server` run in one network namespace and the recorder in another, and netem drops 3% of what the servers send the recorder. The test checks that the recorder's ICE pair uses the link between the namespaces, that lost packets were read later (by sequence number, whichever SSRC carried them), that nearly every frame was written, and that there was no keyframe loop. It needs root and `E2E_NETEM=1`, and is skipped otherwise, or fails with `E2E_NETEM_REQUIRED=1`. |
| `TestNACKRepairControl`, `TestNetemPlan` | The NACK scenario's stack and checks without the namespaces or the loss, with loopback addresses standing in for the links, and the namespace plan and config edits. Both run unprivileged. The control cannot show that ICE uses the link between the namespaces; only the NACK scenario can. |
| `TestSignalCutResumesInPlace` | **Signal cut.** A TCP proxy resets the recorder's signalling connections and lets the redial through. Each session resumes in place, in its one file. |
| `TestSignalRefusedSplitsFile` | **Signal refused.** The proxy also refuses redials for 5 s. The SDK falls back to a full reconnect and every session stops as `closed`. The test retries as the SFU does, so each session ends with two files. This pins today's behaviour; it is not a target. |
| `TestPublisherSendLoop`, `TestVP8PayloadHelpers` | The publisher's send loop (PLI answers, pacing, drops, pauses) and the VP8 payload helpers, with no servers involved. |

## Adding a scenario

A scenario is a `Test` function in a file of its own under `tests/e2e`,
with the `//go:build e2e && linux` header, named after what it checks. Use
this tier for what needs the real path: livekit-server, signalling, ICE or
the network. A deterministic read schedule is cheaper and faster as a seed
of the in-process replay corpus
(`internal/webrtc/livekit/testdata/corpus/README.md`).

1. **Start a stack.** `s := NewStack(t, StackOptions{})` starts
   livekit-server, redis-server and the recorder on free loopback ports,
   and stops them when the test ends. `StackOptions` edits the generated
   configs.
2. **Publish.** `pub := s.NewPublisher(room, identity)`, with a room unique
   to the test (`"e2e-<scenario>-"` plus a short UUID), then
   `pub.PublishVP8` or `pub.PublishOpus` with `TrackOptions`. A track sends
   a `corpus.GenerateVP8` or `corpus.GenerateOpus` schedule. `TrackOptions`
   also sets how the publisher answers a keyframe request (`KeyframeDelay`,
   `KeyframeSpread`) and which packets it never sends (`Drop`). Wait with
   `pub.WaitPublished`.
3. **Record as the SFU does,** one session per track:
   `s.Driver.StartRecording(ctx, room, track.SID)`, then
   `WaitRtpStatus(..., "flowing")`. Record for a fixed time, then `Stop`
   and `WaitStopped`.
4. **Read what came out.** `ReadOutput(t, rec, trackID)` reads the WebM
   through `internal/corpus/oracle` and summarises the recorder's trace
   (see `Output`). `ScrapeMetrics` and `Delta` give the metrics, and
   `SentBetween` what the publisher sent.
5. **Assert bounds derived from the mechanism,** not tuned to a run. Name
   each constant and say where it comes from, as the existing scenarios do
   for the start-up allowance and the frames held at stop. Log what was
   measured with `t.Logf`, so a failure shows its margin.
6. **Prove it can fail.** Put back the defect it guards in the recorder,
   build that recorder, run the scenario against it with
   `E2E_RECORDER_BIN`, and check that it fails. Then run it a few times
   (`-count=3`) to check that its bounds hold.
7. **Register it** with a row in the table above.

A scenario that needs root, as the NACK scenario does for its network
namespaces, skips unless `E2E_NETEM=1` and fails instead when
`E2E_NETEM_REQUIRED=1` (see `netemSkipReason`).

CI runs the whole package unprivileged. A root-only scenario also needs its
own step in `.github/workflows/e2e.yml`, as the NACK scenario has.

## Running locally

You need Go (the version in `go.mod`), `livekit-server` 1.13.7 and
`redis-server`.

```sh
LIVEKIT_SERVER_BIN=/usr/bin/livekit-server \
  go test -tags e2e -v -timeout 15m ./tests/e2e/...

# One scenario, three times
go test -tags e2e -run 'TestBaselineRecording' -count=3 -v ./tests/e2e/...
```

Environment:

| Variable | Effect |
|---|---|
| `LIVEKIT_SERVER_BIN` | The `livekit-server` binary. Default: `livekit-server` on `PATH`. |
| `REDIS_SERVER_BIN` | The `redis-server` binary. Default: `redis-server` on `PATH`. |
| `REDIS_ADDR` | Use this Redis instead of starting one. Channel names get a suffix naming the run (stamp and PID) and the test, so concurrent runs and tests do not cross. The NACK scenario and its control always start their own. |
| `E2E_RECORDER_BIN` | Use this recorder binary instead of building one. |
| `E2E_RECORDER_RACE=1` | Build the recorder with `-race`; a test fails when its recorder's log reports a data race. The race build needs a lot of cache space. |
| `E2E_ARTIFACTS` | Root directory for artifacts. When set, every test's artifacts are kept. |
| `E2E_KEEP_ARTIFACTS=1` | Keep the artifacts of passing tests under the default root. |
| `E2E_RECORDER_LOG_LEVEL`, `E2E_LIVEKIT_LOG_LEVEL` | Log levels. Defaults: `debug` and `info`. |
| `E2E_NETEM=1` | Allow the NACK scenario's privileged path. The test must also run as root. |
| `E2E_NETEM_REQUIRED=1` | Fail the NACK scenario instead of skipping it when the privileged path cannot run. |

The child processes do not inherit `BBBRECORDER_*`, `LIVEKIT_*`, `REDIS_*`,
`NODE_IP` or `UDP_PORT`, so a shell set up for a dev recorder cannot override
the generated configs. Each child runs in its own process group and is
killed when its test ends, and also when the test binary dies on a panic or
a `-timeout`.

### Artifacts

Each test writes to `<run>/<Test>-<n>/`. With `E2E_ARTIFACTS` set, `<run>`
is `$E2E_ARTIFACTS/<stamp>-<pid>`. Otherwise it is a new private directory,
`$TMPDIR/bwr-e2e-*`, and a passing test's directory is removed. A directory
holds:

```
bbb-webrtc-recorder.{yml,log}  livekit-server.{yaml,log}  redis-server.log  lksdk-test-process.log
pubsub.ndjson                                    # every Redis message sent and received
publisher.<identity>.<trackSID>.{sent.ndjson.gz,summary.json}
recorder/recordings/<room>/<trackSID>-<session>.webm
                          /<trackSID>-<session>.<trackSID>.trace.ndjson.gz
                          /<trackSID>-<session>-stats.json
                          /<trackSID>-<session>.summary.json
```

A test run takes a few hundred KB.

### The NACK scenario (root)

The NACK scenario builds two network namespaces and three veth links, with
addresses from three /30s in `198.18.0.0/15` derived from the test's PID:
- the server namespace runs `livekit-server` and `redis-server`, which
  listen on both of its addresses;
- the recorder namespace runs the recorder;
- host <-> server: the test's publisher and Redis driver reach the servers
  here;
- host <-> recorder: only the test's scrape of the recorder's metrics;
- server <-> recorder: the recorder's signalling, Redis and media. netem
  drops 3% of the server end's egress, so only what the servers send the
  recorder is lost.

Every connection that enters the host namespace is opened from the host, and
the replies to it are established traffic; the publisher's own ICE checks
set that up for LiveKit's media. The NACK scenario therefore does not depend
on the host firewall, which may drop new inbound connections. It turns on no
forwarding, adds no route and changes no firewall rule. The publisher's
checks toward the servers' recorder-side address follow the host's default
route and are lost there, as that range is never routed.

The kernel needs the `sch_netem` module. Build both binaries as your user
into a private directory, then run only the test binary as root, with its
artifacts in that directory too:

```sh
d=$(mktemp -d) &&
  go build -o "$d/bwr" ./cmd/bbb-webrtc-recorder &&
  go test -c -tags e2e -o "$d/e2e.test" ./tests/e2e &&
  cd tests/e2e && sudo env "PATH=$PATH" "LIVEKIT_SERVER_BIN=$LIVEKIT_SERVER_BIN" \
    "REDIS_SERVER_BIN=$REDIS_SERVER_BIN" E2E_NETEM=1 E2E_RECORDER_BIN="$d/bwr" \
    E2E_ARTIFACTS="$d/artifacts" "$d/e2e.test" -test.run '^TestNACKRepairUnderLoss$' -test.v
```

`go test -tags e2e -v -run TestNetemPlan ./tests/e2e/...` logs the exact
setup and teardown commands for a sample PID.

Do this only on a disposable machine: it changes the host's network
configuration while it runs. The test removes the namespaces and their
links when it ends, and at startup it removes any that a dead run left
behind.

The NACK scenario's bounds are derived from the loss rate, packets per frame
and the recorder's keyframe requests (see `netem_test.go`). Netem dropping a
packet and every retransmission of it is rare. Most unrepaired losses come
from LiveKit: after a subscriber's PLI, it stops retransmitting on that
downtrack until it forwards a key frame, and it does not forward a PLI
within 500 ms of the previous one. So a dropped request leaves one outage,
until the recorder's next request up to 1 s later. Frames are compared from
the first one written: when netem drops the first packet of the first key
frame, no NACK can name it, and the recording starts only once the jitter
buffer's 2 s have passed.

## CI

`.github/workflows/e2e.yml` runs on `ubuntu-latest`:
- nightly;
- on demand (`workflow_dispatch`);
- on pull requests that touch the recorder (`internal/`, `cmd/`,
  `third_party/`, `tests/e2e/`, `go.mod`, `go.sum` or the workflow itself),
  when opened, pushed to or reopened;
- on any other pull request that carries the `e2e` label: when the label is
  added, on every later push, and when the PR is reopened.

A `changes` job lists the paths a PR changes, against the base it would
merge into. A newer run for a PR cancels the one in progress; a run skipped
for another label does not.

A separate `vet` job runs `go vet -tags e2e ./tests/e2e/...` on every pull
request, labelled or not, and on every other trigger, since the per-push
test workflow builds without the tag. The per-push golangci-lint job lints the
suite with it.

The e2e job sets up Go from `go.mod` and downloads `livekit-server` 1.13.7 from
the official GitHub release, checking it against a pinned sha256. The tarball
is cached under that version and checksum, and checked again on a cache hit.
It installs `redis-server` from apt and stops the system service, then runs
two steps:
1. `go test -tags e2e -v -timeout 15m ./tests/e2e/...`, the whole suite
   unprivileged. The NACK scenario's privileged test skips here.
2. The NACK scenario as root, as shown above, with
   `-test.run '^TestNACKRepairUnderLoss$'` and `E2E_NETEM_REQUIRED=1`, so a
   skip fails the step. This step runs even if step 1 failed, but not if
   setup did, and has its own 10-minute limit. It loads `sch_netem`, and
   installs `linux-modules-extra` for the running kernel if the module is
   missing.

The job runs twice on every trigger, as a matrix: once with the plain
recorder, and once as `Run E2E Tests (race)` with `E2E_RECORDER_RACE=1`,
where the NACK step builds its recorder with `-race` too. A race report in
the recorder's log fails that leg. The race detector does not catch every
race on every run, so a race-leg failure that passes on a rerun is still a
race in the recorder, not a flake.

`E2E_ARTIFACTS` points at `$RUNNER_TEMP/e2e-artifacts`, so every test's
artifacts are kept. When the job fails or is cancelled (as a timeout does),
it uploads them (14-day retention), after a `chown`, because the NACK
scenario writes them as root with mode 0600.

To bump LiveKit, change `LIVEKIT_VERSION` and `LIVEKIT_SHA256` together. The
checksum is the `linux_amd64` line of the release's `checksums.txt`.
