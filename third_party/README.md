# Third-party module copies

## media-sdk

`media-sdk/` holds the `jitter` package of `github.com/livekit/media-sdk`
v0.1.1, the only media-sdk package the recorder imports, plus the
module's `go.mod`, `go.sum` and `LICENSE`. The `replace` directive in `go.mod`
points the module at this copy.

Remove the copy and the `replace` directive once a media-sdk release
carries the local changes below.

Local changes: none.
