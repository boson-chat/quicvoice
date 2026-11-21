# quicvoice's copy of webtransport-go

Vendored from github.com/quic-go/webtransport-go at
v0.13.1-0.20261109070945-2a2b547d91a8 (MIT, see LICENSE), used through a
`replace` in quicvoice's go.mod. Upstream still targets quic-go v0.62;
quicvoice uses v0.65. Drop this copy once upstream supports it.

Patches:

1. `server.go` `Upgrade`: since quic-go v0.63, `HTTPStream()` returns
   `(*Stream, error)`, and calling `WriteHeader` after it panics. The 200
   response is now written (and flushed) before the stream is taken over;
   the multiple-sessions-without-flow-control rejection moved ahead of it
   (it still resets the request stream, as upstream).
2. `send_stream.go`: `SetPriority` passes RFC 9218 priorities through to
   the QUIC stream (quicvoice schedules screen shares before cameras).

Removed: examples, integration/interop tests, CI config.

With these patches the upstream unit tests pass against quic-go v0.65.
