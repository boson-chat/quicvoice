# quicvoice

Voice chat layer (server + client SDK) in Go, using QUIC (`quic-go`) for
transport. No WebRTC. UI is built separately (first consumer: a Wails app).

- Control: one reliable QUIC stream per connection, length-prefixed protobuf.
- Audio: QUIC datagrams carrying 20 ms Opus frames with a small fixed header.
- Topology: client ↔ server only (no P2P). Server forwards, clients mix.

Docs:

- [docs/PLAN.md](docs/PLAN.md) — architecture, milestones, open questions
- [docs/PROTOCOL.md](docs/PROTOCOL.md) — datagram format + control stream
- [proto/control.proto](proto/control.proto) — control message sketch
- [wire/](wire/) — datagram header encode/decode (reference implementation)

```
go test ./...
```
