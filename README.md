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
make run                 # server on :7700/udp (self-signed, AUTH_INSECURE), :8090 public API, :8080 health/metrics
make bot name=alice      # in another terminal
make bot name=bob        # both report the frames they get from each other
make test
```

With `AUTH_INSECURE=true` the token is just the user name (`alice@listener`
sets a role). Otherwise tokens are HS256 JWTs signed with `JWT_SECRET`:

```sh
JWT_SECRET=... go run ./server/cmd token --user u1 --name Alice --room general
# with roles/perms claims, like the auth service's network-role tokens:
JWT_SECRET=... go run ./server/cmd token --user u1 --name alice --network-role DJ --perm voice.create
go run ./examples/bot -addr voice.boson.chat:7700 -token "$TOKEN" -room general
```

## Rooms

Rooms live in a registry: **static** rooms from `VOICE_ROOMS`
(comma-separated `id:Display Name`, in display order; default
`general:General`; ids are lowercase `[a-z0-9-]{1,32}`, a bare `id` uses the
id as its name) plus **dynamic** rooms created through the public API,
optionally under an IRC channel, with join/speak locks on IRC channel roles
(founder, op, voice, …) looked up in Ergo. Who may create them is the auth
service's `voice_channel_creation` setting (`admins | founders | registered`),
plus the voice token's `perms` claim (`voice.create`, `voice.manage_any`,
`voice.bypass_locks`) when it has one; rooms can also be limited to network
roles (`allowed_roles`, matched against the token's `roles` claim).
The registry is persisted in `VOICE_ROOMS_DB` (JSON). See
[docs/ROOMS.md](docs/ROOMS.md) for the model, permissions, the Ergo lookups
and the full API.

With `VOICE_ROOMS_STRICT=true` (the default) a `Join` for any room not in the
registry gets `Error{code: ROOM_NOT_FOUND}`. With `false`, other names create
ad-hoc rooms; they're listed after the registry rooms while someone is in
them. A token's `rooms` claim still limits which rooms a user can join
(`FORBIDDEN`) and see.

## Public HTTP API (`PUBLIC_HTTP_ADDR`, default `:8090`)

Separate from the internal `:8080` listener, so `/metrics` is never
exposed. Put it behind a TLS reverse proxy (e.g. `https://voice.boson.chat`).
Every `/v1` route takes `Authorization: Bearer <voice token>`, verified
exactly like the QUIC `Hello` (same `aud`/`iss`/JWKS); full request/response
reference in [docs/ROOMS.md](docs/ROOMS.md#api).

| Route | |
|---|---|
| `GET /healthz` | `ok` |
| `GET /v1/rooms` | Room list with live participants, each room's `parent`, `owner`, locks (incl. `allowed_roles`) and the caller's `can_manage`/`can_join`/`can_speak`, plus top-level `permissions` (`can_create`, `create_scope`, `can_manage_any`, `bypass_locks`) |
| `POST /v1/rooms` | Create a room `{name, parent?, min_role?, speak_role?, require_channel_membership?, allowed_roles?}` → `201` |
| `PATCH /v1/rooms/{id}` | Rename / reorder / re-lock `{name?, position?, min_role?, speak_role?, require_channel_membership?, allowed_roles?}` → `200` |
| `DELETE /v1/rooms/{id}` | Delete; everyone in it gets `Error{KICKED}` → `204` |
| `GET /.well-known/video-jwks.json` | Public key for video tickets (only with `VIDEO_QUIC_URL`; no auth). See [docs/VIDEO.md](docs/VIDEO.md) |
| `GET /v1/info` | `{quic_port, webtransport_path: "/wt", dev_cert_hash?}` (no auth). `dev_cert_hash` (hex SHA-256) only with the self-signed dev certificate, for browsers' `serverCertificateHashes` |

Browsers may call the API from the pages in `CORS_ALLOWED_ORIGINS`
(preflights answered for `GET/POST/PATCH/DELETE` with `Authorization` and
`Content-Type`; never with credentials: the API takes bearer tokens only).

Missing, malformed, invalid or expired token → `401`,
`WWW-Authenticate: Bearer realm="quicvoice"`, body `{"error":"...","code":"unauthorized"}`.

## Config (env)

| Var | Default | |
|---|---|---|
| `QUIC_ADDR` | `:7700` | UDP voice listener |
| `HTTP_ADDR` | `:8080` | `/healthz`, `/version`, `/metrics`, `/internal/config/status`. Cluster-internal only |
| `PUBLIC_HTTP_ADDR` | `:8090` | Public API: `GET /v1/rooms`, `GET /healthz`. Empty = off |
| `WEBTRANSPORT_ORIGINS` | — (any) | Pages allowed to open WebTransport sessions (`https://host:7700/wt`), comma-separated, e.g. `https://chat.boson.chat`. Empty or `*` = any page (the token still authorizes). `directory` = the list at `ORIGINS_URL` (see [web origins](#web-origins-directory)); mixable, e.g. `directory,https://chat.example.com`. Origins compare normalised (case, default port, trailing slash). Refused origins get `403` |
| `ORIGINS_URL` | — | With `directory`: the auth service's `GET /v1/web-origins` (`{"origins": [...]}`), e.g. `http://auth:8080/v1/web-origins`. Required when a list says `directory` |
| `ORIGINS_REFRESH` | `5m` | How often `ORIGINS_URL` is fetched (every 30 s, or this if shorter, after a failure) |
| `CORS_ALLOWED_ORIGINS` | `WEBTRANSPORT_ORIGINS` | Pages allowed to call the public API (CORS), comma-separated; `*` = any; `directory` as above. Empty = same as `WEBTRANSPORT_ORIGINS`, so any page when both are empty |
| `VOICE_ROOMS` | `general:General` | Configured rooms, comma-separated `id:Display Name`, in display order. Ids: `[a-z0-9-]{1,32}`. `voice.rooms` in the config file wins |
| `VOICE_ROOMS_STRICT` | `true` | Only registry rooms can be joined (`ROOM_NOT_FOUND` otherwise). `false` = other names create ad-hoc rooms |
| `VOICE_ROOMS_DB` | — | Room registry file (e.g. `/data/rooms.json`). Empty = in memory: API-created rooms are lost on restart |
| `AUTH_INTERNAL_URL` / `INTERNAL_API_TOKEN` | — | Auth service internal API, for the `voice_channel_creation` setting (cached `SETTINGS_TTL`, 30s) |
| `VOICE_CHANNEL_CREATION` | `admins` | `admins` \| `founders` \| `registered`, used until the first settings fetch succeeds (and always without `AUTH_INTERNAL_URL`) |
| `ERGO_API_URL` / `ERGO_API_TOKEN` | — | Ergo HTTP API for founder checks and channel roles (cached `ERGO_CACHE_TTL`, 15s). Empty = no IRC roles |
| `VOICE_ROOMS_MAX_PER_USER` / `VOICE_ROOMS_MAX` | 10 / 500 | Dynamic room limits (per non-admin owner / total) |
| `TLS_CERT_FILE` / `TLS_KEY_FILE` | — | Reloaded when the file changes. Unset = self-signed |
| `AUTH_JWKS_URL` | — | Verify tokens from the boson auth service (EdDSA/ES256/RS256) via its JWKS, e.g. `https://auth.example.com/.well-known/jwks.json`. Keys are cached and refetched on an unknown `kid` |
| `AUTH_ISSUER` | — | Required `iss` for JWKS tokens (recommended) |
| `AUTH_AUDIENCE` | `quicvoice` | Required `aud` for JWKS tokens |
| `JWT_SECRET` | — | HS256 shared secret (legacy / `quicvoiced token`). Can be set together with `AUTH_JWKS_URL`; tokens are routed by `alg`. One of the two is required unless `AUTH_INSECURE=true` |
| `ROOM_MAX_PARTICIPANTS` | 50 | |
| `MAX_DATAGRAMS_PER_SEC` | 60 | Per sender |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | Set to turn on OTLP metrics push (e.g. `http://otel-collector:4318`; `/v1/metrics` is appended for HTTP). `/metrics` is served either way |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | — | Full metrics URL; overrides the above and also turns push on |
| `OTEL_EXPORTER_OTLP_PROTOCOL` / `_METRICS_PROTOCOL` | `http/protobuf` | Or `grpc` |
| `OTEL_METRICS_EXPORTER` | — | `none` turns OTLP push off even with an endpoint set |
| `OTEL_METRIC_EXPORT_INTERVAL` | 60000 | OTLP push interval, ms. Other `OTEL_EXPORTER_OTLP_*` vars (headers, timeout, TLS, temporality) and `OTEL_SERVICE_NAME` / `OTEL_RESOURCE_ATTRIBUTES` work as usual |
| `QUIC_STATS_INTERVAL` | `15s` | How often each connection's QUIC RTT/loss is sampled. `0` = only at close |
| `VIDEO_QUIC_URL` / `VIDEO_WEBTRANSPORT_URL` | — | Video for voice channels via quicvideod. Empty = off. With `VIDEO_TICKET_KEY_FILE`, `VIDEO_TICKET_ISSUER`, `VIDEO_TICKET_TTL`, `VIDEO_CONTROL_URL`, `VIDEO_CONTROL_TOKEN`: see [docs/VIDEO.md](docs/VIDEO.md#config) |
| `BOSON_CONFIG` | — | The boson stack config file (e.g. `/config/boson.yaml`), see below. Unset or missing file = env only |

## Config file (`BOSON_CONFIG`, live reload)

In the boson stack every service shares one non-secret YAML file
(`config/boson.yaml`, written by the admin panel or by hand). quicvoiced reads
only its `voice:` section and ignores the rest. A key in the file beats the
env var, which beats the default; an absent key means "use env / default", so
removing a key resets it.

```yaml
version: 1
voice:
  # live
  rooms:                       # VOICE_ROOMS; [] = no static rooms
    - {id: general, name: General}
    - music                    # bare id: name = id
    - {id: stage, name: Stage, max_kbps: 128}  # this room's voice bitrate cap
  rooms_strict: true           # VOICE_ROOMS_STRICT
  max_rooms_per_user: 10       # VOICE_ROOMS_MAX_PER_USER
  max_rooms: 500               # VOICE_ROOMS_MAX
  max_participants: 50         # ROOM_MAX_PARTICIPANTS (new joins)
  max_kbps: 64                 # voice bitrate cap (kbit/s, 6..510) in rooms
                               # without their own; absent/0 = no cap. Sent to
                               # clients in RoomState.max_bitrate (and
                               # RoomSettings when it changes); they encode at
                               # min(their quality setting, cap). Not enforced.
  settings_ttl: 30s            # SETTINGS_TTL
  ergo_cache_ttl: 15s          # ERGO_CACHE_TTL
  # restart required
  quic_addr: ":7700"           # QUIC_ADDR
  http_addr: ":8080"           # HTTP_ADDR
  public_http_addr: ":8090"    # PUBLIC_HTTP_ADDR ("" = off)
  max_datagrams_per_sec: 60    # MAX_DATAGRAMS_PER_SEC
  max_datagram_payload: 1000   # MAX_DATAGRAM_PAYLOAD
  web_origins:                 # WEBTRANSPORT_ORIGINS + CORS_ALLOWED_ORIGINS
    - https://chat.example.com  # ([] = any page; "directory" = ORIGINS_URL's list)
```

### Web origins (directory)

`WEBTRANSPORT_ORIGINS=directory` (the Boson stack's default) accepts the
pages of the Boson web clients the install's auth service lists at
`GET /v1/web-origins`: its own web client plus those of the Boson servers
verified in the directories it follows (auth rebuilds that every 10 min,
from each server's boson.json `web.url`). quicvoiced fetches `ORIGINS_URL`
at start and every `ORIGINS_REFRESH` and applies it to both the
WebTransport `Origin` check and the public API's CORS (an allowed page gets
its origin echoed back, never `*`). A failed fetch keeps the last good list;
until the first one works only the fixed entries next to `directory` are
allowed, which is why the stack passes `directory,https://WEB_HOST`. So a
newly verified server's web client gets in within about 15 minutes.

The file is watched (fsnotify on its directory, since the admin panel
replaces it atomically, plus an mtime poll every 10 s). On a change:

- **valid**: applied live and logged as `config reloaded`. Static rooms are
  added, renamed, reordered and removed in the registry; everyone in a removed
  room gets `KICKED` ("voice channel "Music" was removed from the server
  config"), like a deleted dynamic room. Turning `rooms_strict` on refuses new
  joins to ad-hoc rooms but doesn't kick people already in one; lower limits
  apply to new joins/creations.
- **invalid**: the last good config stays; the error (with line and column)
  is logged and served on the status endpoint.
- keys in the "restart required" group are reported until quicvoiced is
  restarted with them (or they're changed back).

Mount the config **directory** (e.g. `./config:/config:ro`), not the single
file: a single-file bind mount keeps the old inode after an atomic replace and
never sees the change.

`GET :8080/internal/config/status` (internal listener):

```json
{"path":"/config/boson.yaml","loaded_at":"2026-11-14T10:00:00Z","file_mtime":"2026-11-14T09:59:59Z",
 "ok":false,"error":"line 4, column 11: voice.rooms: invalid room id \"Bad\" (want lowercase [a-z0-9-]{1,32})",
 "error_line":4,"error_column":11,"restart_required":["voice.quic_addr"]}
```

`loaded_at`: when the config in use was applied (`null` if the file was
invalid from the start: quicvoiced then runs on env); `file_mtime`: `null`
when the file is missing; `error*` only when `ok` is false.

`quicvoiced export-config` prints a `voice:` section from the current env
(and the file, if `BOSON_CONFIG` is set) to merge into the file on an
existing install: `rooms`, `rooms_strict` and `max_rooms_per_user` always,
the other keys only when their env var is set.

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
| `quicvoice.room.joins` (`quicvoice_room_joins_total`) | counter, `{join}` | `outcome` = ok, full, forbidden, bad_request, already_in_room, not_found | Are people being turned away? (`full` → raise `ROOM_MAX_PARTICIPANTS`; `forbidden` → token room list; `not_found` → room not in `VOICE_ROOMS`) |
| `quicvoice.room.leaves` (`quicvoice_room_leaves_total`) | counter, `{leave}` | `reason` = left, disconnected, kicked | Do people leave on purpose, or drop out? (`kicked` = their room was deleted) |
| `quicvoice.room.changes` (`quicvoice_room_changes_total`) | counter, `{change}` | `op` = create, update, delete; `outcome` = ok, forbidden, invalid, conflict, not_found, limit, unavailable, error | Are people creating/deleting voice channels, and why are requests refused? `unavailable` = Ergo unreachable for a founder/role check |
| `quicvoice.rooms.defined` (`quicvoice_rooms_defined`) | gauge, `{room}` | `kind` = static, dynamic | How many rooms are in the registry? |
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
- [docs/ROOMS.md](docs/ROOMS.md): room registry, creation policy, IRC role locks, rooms API
- [docs/VIDEO.md](docs/VIDEO.md): video tickets and the control link to quicvideod

## License

Copyright (C) 2026 bludot / the boson authors

Licensed under the [GNU Affero General Public License v3.0](LICENSE) only (`AGPL-3.0-only`).

In plain English: if you run a modified version of this software as a network service, you must offer its source code to the people who use that service.
