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
