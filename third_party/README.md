# Third-party module copies

The recorder needs local changes to these modules. A `replace` directive in
`go.mod` points each module at its copy.

## media-sdk

`media-sdk/` holds the `jitter` package of `github.com/livekit/media-sdk`
v0.1.1, the only media-sdk package the recorder imports, plus the
module's `go.mod`, `go.sum` and `LICENSE`.

Remove the copy and the `replace` directive once a media-sdk release
carries the local changes below.

Local changes:

- `jitter/buffer.go`, `dropIncomplete`: an incomplete head packet that
  arrived exactly at the expiry counts as expired, as it already does for
  complete frames in `popReady` (`TestIncompleteExpiresAtDeadline` in
  `jitter/buffer_test.go`). Without it, a timer that fires exactly
  at the deadline drops nothing and re-arms itself for zero, which never
  ends under `testing/synctest`.
- `jitter/packet.go`, `newPacket`: upstream's 1420485, applied verbatim. A
  recycled packet entry starts with its `discont` flag cleared, so a frame
  behind an open gap is not released as the start of a new stream.

## samplebuilder

`samplebuilder/` holds `github.com/jech/samplebuilder` at
v0.0.0-20221109182433-6cbba09fc1c9: its package, tests, `go.mod`, `go.sum`
and `LICENCE`.

Remove the copy and the `replace` directive once a samplebuilder release
carries the local changes below.

Local changes: none.
