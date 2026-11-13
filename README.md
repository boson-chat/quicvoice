# quicvoice

Voice chat over QUIC (`quic-go`) in Go: a server, a client SDK and test
clients. No WebRTC.

- Control: one reliable QUIC stream per connection, length-prefixed protobuf.
- Audio: QUIC datagrams carrying 20 ms Opus frames with a small fixed header.
- Topology: client ↔ server only (no P2P). The server forwards, clients mix.

## Layout

```
proto/         control.proto (source of truth) → gen/controlpb
wire/          datagram + control framing shared by server and client
server/        quicvoiced (cmd/, config/, internal/{commands,services,transport}, http/, metrics/)
client/        Go SDK: Dial, Join, SendAudio, SetMute, OnAudio/OnControl
examples/bot   headless test client (synthetic frames; load tests)
```

## Run locally

```sh
make run                 # server on :7700/udp (self-signed, AUTH_INSECURE) + :8080 health/metrics
make bot name=alice      # in another terminal
make bot name=bob        # both report the frames they get from each other
make test
```

With `AUTH_INSECURE=true` the token is just the user name (`alice@listener`
sets a role). Otherwise tokens are HS256 JWTs signed with `JWT_SECRET`:

```sh
JWT_SECRET=... go run ./server/cmd token --user u1 --name Alice --room lobby
go run ./examples/bot -addr voice.boson.chat:7700 -token "$TOKEN" -room lobby
```

## Config (env)

| Var | Default | |
|---|---|---|
| `QUIC_ADDR` | `:7700` | UDP voice listener |
| `HTTP_ADDR` | `:8080` | `/healthz`, `/version`, `/metrics` |
| `TLS_CERT_FILE` / `TLS_KEY_FILE` | — | Reloaded when the file changes. Unset = self-signed |
| `JWT_SECRET` | — | Required unless `AUTH_INSECURE=true` |
| `ROOM_MAX_PARTICIPANTS` | 50 | |
| `MAX_DATAGRAMS_PER_SEC` | 60 | Per sender |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | Set to turn on OTLP metrics push (e.g. `http://otel-collector:4318`; `/v1/metrics` is appended for HTTP). `/metrics` is served either way |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | — | Full metrics URL; overrides the above and also turns push on |
| `OTEL_EXPORTER_OTLP_PROTOCOL` / `_METRICS_PROTOCOL` | `http/protobuf` | Or `grpc` |
| `OTEL_METRICS_EXPORTER` | — | `none` turns OTLP push off even with an endpoint set |
| `OTEL_METRIC_EXPORT_INTERVAL` | 60000 | OTLP push interval, ms. Other `OTEL_EXPORTER_OTLP_*` vars (headers, timeout, TLS, temporality) and `OTEL_SERVICE_NAME` / `OTEL_RESOURCE_ATTRIBUTES` work as usual |
| `QUIC_STATS_INTERVAL` | `15s` | How often each connection's QUIC RTT/loss is sampled. `0` = only at close |

## Metrics

OpenTelemetry metrics (Go SDK). They're always served as Prometheus text on
`GET :8080/metrics`, and also pushed over OTLP when an OTLP endpoint is set
(see Config). The resource has `service.name=quicvoiced`, `service.version`
(`VERSION`), `deployment.environment` and `deployment.environment.name`
(`ENV`) and `host.name`. In Prometheus these show up on `target_info`.

OTel names are shown first, then the Prometheus name: dots become `_`, a unit
suffix is added, and counters get `_total`. Attribute values come from fixed
sets. Room names, user ids and addresses are never attributes.

**Capacity**

| Metric (Prometheus name) | Type | Attributes | Answers |
|---|---|---|---|
| `quicvoice.connections` (`quicvoice_connections`) | up-down counter, `{connection}` | — | How many QUIC connections are open, including ones that haven't authenticated yet? |
| `quicvoice.sessions` (`quicvoice_sessions`) | up-down counter, `{session}` | `client.version` | How many authenticated users are connected, and on which client builds? (`connections − sessions` = connections stuck before Hello) |
| `quicvoice.rooms` (`quicvoice_rooms`) | gauge, `{room}` | — | How many rooms are live? |
| `quicvoice.participants` (`quicvoice_participants`) | gauge, `{participant}` | `role` = unspecified, listener, speaker, moderator, admin | How many people are in rooms, and how many of them can talk? |
| `quicvoice.room.size.max` (`quicvoice_room_size_max`) | gauge, `{participant}` | — | How close is the biggest room to `ROOM_MAX_PARTICIPANTS`? |
| `quicvoice.room.size` (`quicvoice_room_size`) | histogram, `{participant}` | — | How big are rooms when people join? Sampled once per successful join, so busy rooms weigh more |
| `quicvoice.audio.speech` (`quicvoice_audio_speech_seconds_total`) | counter, `s` | — | How much talking is going on? `rate()` of it = average number of people talking at once (forwarded frames with the VAD bit × 20 ms) |
| `quicvoice.datagrams.io` (`quicvoice_datagrams_io_bytes_total`) | counter, `By` | `network.io.direction` = receive, transmit | How much audio payload goes in and out? |
| `quicvoice.quic.io` (`quicvoice_quic_io_bytes_total`) | counter, `By` | `network.io.direction` | Actual QUIC bandwidth incl. packet overhead, keepalives and control (excluding UDP/IP headers). Sampled per connection, so it lags by up to `QUIC_STATS_INTERVAL` |

**Audio path / "why can't X hear Y"**

| Metric | Type | Attributes | Answers |
|---|---|---|---|
| `quicvoice.datagrams.received` (`quicvoice_datagrams_received_total`) | counter, `{datagram}` | `kind` = uplink, ping, other | Are clients sending audio? Counted before rate limiting |
| `quicvoice.audio.frames.forwarded` (`quicvoice_audio_frames_forwarded_total`) | counter, `{frame}` | — | How many uplink frames passed every check and were fanned out? |
| `quicvoice.datagrams.sent` (`quicvoice_datagrams_sent_total`) | counter, `{datagram}` | `kind` = downlink, pong | How many downlinks went out? `rate(sent{kind="downlink"}) / rate(frames_forwarded)` = average fan-out |
| `quicvoice.datagrams.dropped` (`quicvoice_datagrams_dropped_total`) | counter, `{datagram}` | `reason` = malformed, rate_limited, too_large, not_in_room, not_allowed, target_unsupported, unknown_kind, send_too_large, send_failed | Where is audio being thrown away? `not_allowed` = listener, or muted; `not_in_room` = sending before Join; `send_too_large` = the receiver's path MTU is too small for the downlink; `send_failed` = the receiver's connection is gone. Send-side reasons count once per receiver |
| `quicvoice.audio.uplink.lost` (`quicvoice_audio_uplink_lost_total`) | counter, `{frame}` | — | Is audio being lost between clients and the server? Estimated from gaps in each sender's sequence numbers. Gaps over 1 s and talk-spurt starts aren't counted. Compare with `datagrams_received{kind="uplink"}` |

**Network quality** (each authenticated connection is sampled every `QUIC_STATS_INTERVAL` and again when it closes, from quic-go's `ConnectionStats`)

| Metric | Type | Attributes | Answers |
|---|---|---|---|
| `quicvoice.quic.rtt` (`quicvoice_quic_rtt_seconds`) | histogram, `s` | — | What RTTs do users have? (smoothed RTT; buckets 5 ms – 2 s) |
| `quicvoice.quic.rtt.variation` (`quicvoice_quic_rtt_variation_seconds`) | histogram, `s` | — | How jittery are their paths? (RTT mean deviation) |
| `quicvoice.quic.packet_loss` (`quicvoice_quic_packet_loss`) | histogram, `1` | — | How many connections see downlink loss, and how bad is it? Fraction of the server's packets declared lost in each sample window. Only windows with ≥ 50 packets sent count |
| `quicvoice.quic.packets` (`quicvoice_quic_packets_total`) | counter, `{packet}` | `network.io.direction` | Packet rates. Divide lost by transmit to get the overall loss rate |
| `quicvoice.quic.packets.lost` (`quicvoice_quic_packets_lost_total`) | counter, `{packet}` | — | Packets the server sent that were declared lost |

**Sessions, auth, abuse**

| Metric | Type | Attributes | Answers |
|---|---|---|---|
| `quicvoice.handshakes` (`quicvoice_handshakes_total`) | counter, `{handshake}` | `outcome` = ok, no_stream, no_hello, protocol, bad_token, expired | Are logins failing? `expired` spikes = app backend token TTL/clock problems; `bad_token` = wrong `JWT_SECRET` or abuse; `no_stream`/`no_hello` = scanners or broken clients |
| `quicvoice.handshake.duration` (`quicvoice_handshake_duration_seconds`) | histogram, `s` | — | How long from QUIC accept to Welcome? (successful handshakes only) |
| `quicvoice.session.duration` (`quicvoice_session_duration_seconds`) | histogram, `s` | `reason` = client_closed, idle_timeout, slow_consumer, protocol_error, server_shutdown, error | How long do sessions last, and how do they end? Lots of `idle_timeout` = clients losing network; `slow_consumer` = control queue full (we disconnected them) |
| `quicvoice.room.joins` (`quicvoice_room_joins_total`) | counter, `{join}` | `outcome` = ok, full, forbidden, bad_request, already_in_room | Are people being turned away? (`full` → raise `ROOM_MAX_PARTICIPANTS`; `forbidden` → token room list) |
| `quicvoice.room.leaves` (`quicvoice_room_leaves_total`) | counter, `{leave}` | `reason` = left, disconnected | Do people leave on purpose, or drop out? |
| `quicvoice.control.messages` (`quicvoice_control_messages_total`) | counter, `{message}` | `type` = hello, join, leave, set_mute, ping, unsupported | Control-plane load; `unsupported` = newer clients using messages this server ignores |
| `quicvoice_datagrams_dropped_total{reason="rate_limited"}` | (above) | | Is someone flooding? |

Bounded series (connections, handshakes, joins, leaves, control messages,
the QUIC counters) are exported as 0 from startup, so `rate()` and alerts
work before the first event.

`client.version` is Hello's `client_version`, lowercased, with only
`[a-z0-9._/+-]` kept (spaces become `_`) and cut to 48 chars. The first 32
distinct values get their own series and the rest are `other`. An empty
value is `unknown`.

Go runtime metrics (`go_memory_*`, `go_goroutine_count`,
`go_config_gogc_percent`, `go_processor_limit`) come from
`go.opentelemetry.io/contrib/instrumentation/runtime`.

Hot-path cost: per-datagram counters are plain atomics, reported through
observable instruments when a reader collects. A datagram never allocates for
metrics and never goes through the SDK (`TestHotPathDoesNotAllocate`).

## Release + deploy

Conventional commits on `main` → semantic-release tags `vX.Y.Z` and pushes
`harbor.floret.dev/boson/quicvoice:X.Y.Z` (+ `latest`). ArgoCD image-updater
on terra deploys it (`quicvoice/` in `bludot/floret_argocd`).

Production: `voice.boson.chat:7700/udp` → edge → MetalLB `192.168.9.85`
(dedicated, `externalTrafficPolicy: Local`) → one pod. Istio isn't involved;
it only proxies TCP.

## Docs

- [docs/PLAN.md](docs/PLAN.md): architecture, milestones, open questions
- [docs/PROTOCOL.md](docs/PROTOCOL.md): datagram format and control stream
