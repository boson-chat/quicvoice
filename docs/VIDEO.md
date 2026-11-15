# Video in voice channels

Video (camera + screen share) runs on a separate relay,
[quicvideod](https://github.com/boson-chat/quicvideo), which can live on the
same box or on its own. Each voice channel has a video room with the same
id. quicvoiced decides who may be in it; quicvideod only checks a ticket.

```
client ── VideoTicketRequest ──▶ quicvoiced ── VideoTicket{token, urls} ──▶ client
client ── Hello{token} ──▶ quicvideod  (verifies against quicvoiced's /.well-known/video-jwks.json)
quicvoiced ── leave / close events, membership snapshots ──▶ quicvideod control API
```

## Tickets

On the control stream, a session in a voice channel sends
`VideoTicketRequest{}` and gets back
`VideoTicket{token, expires_at, quic_url, webtransport_url, room}`. Not in a
channel → `Error{FORBIDDEN, "join a voice channel first"}`. Video off (no
`VIDEO_QUIC_URL`) → `Error{FORBIDDEN, "video isn't enabled on this network"}`.

The token is an EdDSA JWT:

| Claim | |
|---|---|
| `sub`, `name` | The voice identity's user id and display name |
| `iss` | `VIDEO_TICKET_ISSUER` (default `voice`) |
| `aud` | `quicvideo` |
| `rooms` | `[<voice channel id>]` |
| `perms` | `["video.publish"]` if the user can speak in the channel, else `[]` (watch only) |
| `exp` | `VIDEO_TICKET_TTL` (default 5 minutes) from now |

The ticket is only needed to join, and for quicvideod's token refresh:
clients ask for a new one before `expires_at` and send it as `Refresh`.
Leaving the channel stops new tickets; the control link removes the user
from the video room straight away.

The signing key is an Ed25519 seed in `VIDEO_TICKET_KEY_FILE` (PEM, created
0600 on first start). Keep it on a volume: a new key invalidates tickets in
flight (quicvideod refetches the JWKS on an unknown `kid`, so it recovers on
the next join). The public half is served at
`GET /.well-known/video-jwks.json` on the public API (`Cache-Control:
max-age=300`). quicvideod takes it as one of its `AUTH_JWKS_URL`s with
`AUTH_ISSUER` including `voice`.

## Control link

With `VIDEO_CONTROL_URL` set, quicvoiced pushes membership to quicvideod's
control listener (`CONTROL_HTTP_ADDR`), with `Authorization: Bearer
$VIDEO_CONTROL_TOKEN` (quicvideod's `CONTROL_TOKEN`):

- `POST /v1/control/events` `{"events":[{"type":"leave","room","user"},{"type":"close","room"}]}`
  when a user's last session leaves a channel, or a channel is deleted or
  closed. Batched, retried with backoff (up to 30 s) while quicvideod is
  down; at most 10 000 queued.
- `PUT /v1/control/rooms` `{"rooms":{"<id>":["<user>", …]}}` every 30 s, a
  full snapshot of the channels with anyone in them. quicvideod removes
  video members missing from it after a 15 s grace, so anything the events
  missed (a restart on either side) heals. Empty channels aren't listed, so
  run quicvideod with `VOICE_SYNC=true` (video rooms not in the snapshot are
  emptied too) whenever all its rooms are voice channels, as in the stack.

The link is outbound from quicvoiced, so a remote quicvideod only needs its
control listener reachable from the voice server (e.g. behind TLS at
`https://video.example.com/control`).

## Config

| Var | Default | |
|---|---|---|
| `VIDEO_QUIC_URL` | — | quicvideod's QUIC address handed to clients (e.g. `quic://video.boson.chat:7710`). Empty = video off |
| `VIDEO_WEBTRANSPORT_URL` | — | Its WebTransport URL for browsers (e.g. `https://video.boson.chat:7710/wt`) |
| `VIDEO_TICKET_KEY_FILE` | — | Ed25519 key file (created if missing). Empty = in memory (dev only) |
| `VIDEO_TICKET_ISSUER` / `VIDEO_TICKET_TTL` | `voice` / `5m` | |
| `VIDEO_CONTROL_URL` / `VIDEO_CONTROL_TOKEN` | — | quicvideod's control listener. Empty = no link (tickets still work; leaving voice doesn't kick from video until the ticket expires) |

## Go client

`client.Client.VideoTicket(ctx)` sends the request and returns the
`*controlpb.VideoTicket` (or the server's error).
