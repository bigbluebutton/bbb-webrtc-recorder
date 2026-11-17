# Third-party module copies

The recorder needs local changes to these modules. A `replace` directive in
`go.mod` points each module at its copy.

## samplebuilder

`samplebuilder/` holds `github.com/jech/samplebuilder` at
v0.0.0-20221109182433-6cbba09fc1c9: its package, tests, `go.mod`, `go.sum`
and `LICENCE`.

Remove the copy and the `replace` directive once a samplebuilder release
carries the local changes below.

Local changes:

- `samplebuilder.go`, `Push`: when making room for a packet drops every
  packet the builder holds, the packet goes in as the first one. Without
  it, the packet lands `count` slots after the reset head, and the tail
  stays on an empty slot.
