# Third-party module copies

## media-sdk

`media-sdk/` holds the `jitter` package of `github.com/livekit/media-sdk`
v0.1.1, the only media-sdk package the recorder imports, plus the
module's `go.mod`, `go.sum` and `LICENSE`. The `replace` directive in `go.mod`
points the module at this copy.

Remove the copy and the `replace` directive once a media-sdk release
carries the local changes below.

Local changes:

- `jitter/buffer.go`, `dropIncomplete`: an incomplete head packet that
  arrived exactly at the expiry counts as expired, as it already does for
  complete frames in `popReady` (`TestIncompleteExpiresAtDeadline` in
  `jitter/buffer_test.go`). Without it, a timer that fires exactly
  at the deadline drops nothing and re-arms itself for zero, which never
  ends under `testing/synctest`.
