# Protocol (draft v1)

## Connection

- QUIC (RFC 9000) with the datagram extension (RFC 9221), via `quic-go`.
- ALPN: `quicvoice/1`. Server on UDP 443 by default (gets through most firewalls).
- TLS 1.3 from QUIC handles encryption and integrity, so there's no
  per-packet crypto in our format. (E2EE is a later layer on the Opus
  payload, see "Future".)
- Keepalive: QUIC `KeepAlivePeriod` = 10s (keeps NAT bindings open). Idle
  timeout 30s.
- Connection migration (wifi → cellular) comes from QUIC for free.
- **One voice room per connection at a time** in v1 (Discord model). This
  keeps room IDs out of every datagram.

The client opens **one bidirectional stream** right after the handshake. This
is the control stream. Audio goes only in datagrams.

## Control stream framing

```
[varint length][protobuf ControlMessage]...
```

See `proto/control.proto`. Flow:

```
C → S  Hello{token, client_version, capabilities}
S → C  Welcome{codec, server caps}        | Error + close
C → S  Join{room}
S → C  RoomState{your session_id, participants[]}
...    ParticipantJoined/Left/Updated, SetMute, SetVoiceTarget, Moderate, Ping/Pong
```

The `token` is a short-lived JWT issued by the app backend. It carries user
id, display name, allowed room(s) and permissions (speak, moderate).

`Join{room}` takes a room id. Rooms are configured on the server
(`VOICE_ROOMS`); with `VOICE_ROOMS_STRICT` on (the default) an unknown id
gets `Error{code: ROOM_NOT_FOUND}`, and a room the token's `rooms` claim
excludes gets `Error{code: FORBIDDEN}`. Neither is fatal. Clients discover
rooms and who is in them with `GET https://<voice host>/v1/rooms` (see the
README) before connecting.

## Audio datagrams

All multi-byte integers are big-endian. Opus payload follows the header
directly. Datagrams should stay ≤ ~1100 bytes (QUIC's minimum path MTU is
1200); a 20 ms Opus frame at 32–64 kbps is ~80–160 bytes.

### Byte 0: version + kind (all datagrams)

```
 7 6 5 4 3 2 1 0
+-+-+-+-+-+-+-+-+
|  ver  |  kind |
+-+-+-+-+-+-+-+-+
ver  = 1
kind = 0x0 audio uplink   (client → server)
       0x1 audio downlink (server → client)
       0x2 ping           (either direction, echoed)
       0x3–0xF reserved (receivers drop unknown kinds)
```

### Audio uplink (client → server), 10-byte header

```
 0               1               2               3
 0 1 2 3 4 5 6 7 0 1 2 3 4 5 6 7 0 1 2 3 4 5 6 7 0 1 2 3 4 5 6 7
+-------+-------+---------------+-+-------------+---------------+
|ver=1  |kind=0 |     flags     |V|   level     |    target     |
+-------+-------+---------------+-+-------------+---------------+
|           sequence            |                               |
+-------------------------------+                               +
|                           timestamp                           |
+               +-----------------------------------------------+
|               |  opus payload ...
+---------------+
```

There's no sender ID because the QUIC connection already identifies who sent it.

| Field | Size | Meaning |
|---|---|---|
| flags | u8 | bit0 `START`: first frame of a talk spurt (resets jitter buffer). bit1 `END`: last frame (PTT released / VAD off). bit2 `LOST_PREV`: encoder lost frames before this (debug). Others: 0. |
| V | 1 bit | Voice activity from the sender's VAD |
| level | 7 bits | Audio level in −dBov, 0 = loudest, 127 = silence (same meaning as RFC 6464). Computed on the frame *before* encoding |
| target | u8 | 0 = whole room. 1–31 = voice target set up via `SetVoiceTarget` (whisper / sub-group). Others reserved |
| sequence | u16 | +1 per frame, wraps. Per sender |
| timestamp | u32 | Sample clock at 48 kHz (20 ms = +960). Wraps after ~24.8 h. Random start value |

### Audio downlink (server → client), 12-byte header

The server takes the uplink header, inserts the sender, and rewrites `target`
into a target *kind* for the receiver:

```
 0               1               2               3
+-------+-------+---------------+-+-------------+---------------+
|ver=1  |kind=1 |     flags     |V|   level     |  target_kind  |
+-------+-------+---------------+-+-------------+---------------+
|          session_id           |           sequence            |
+-------------------------------+-------------------------------+
|                           timestamp                           |
+---------------------------------------------------------------+
|  opus payload ...
```

| Field | Meaning |
|---|---|
| target_kind | 0 = normal room audio, 1 = whispered to you directly, 2 = sent to a group you're in. Lets the UI show "X is whispering" |
| session_id | u16 room-local participant ID, announced in `RoomState` / `ParticipantJoined` |

Other fields are passed through unchanged. **The server never rewrites seq
or timestamp.** Each sender is its own stream on the client (jitter buffer
keyed by `session_id`). This is why top-N forwarding is simple here: the
server just stops forwarding a sender, the client sees a gap, and when they
come back `START`, or a big seq jump, resets that sender's buffer. A WebRTC
server has to splice streams together and rewrite seq/timestamp for this.

### Ping (either direction), 9-byte datagram

```
| ver/kind=2 (u8) | flags (u8: bit0 = reply) | id (u16) | sent_us (u32) | pad (u8, 0) |
```

The receiver echoes it back with `reply` set and the other fields
unchanged. The sender uses the result for RTT/jitter/loss stats (connection
quality UI). The sent time `sent_us` is the sender's own clock in µs, wrapping; only the sender
interprets it. Sent every 2 s. QUIC's own RTT estimate is also exposed by
quic-go, but this one measures the datagram path end to end and also works
over the TCP fallback.

## Codec defaults (sent in `Welcome.codec`)

- Opus, 48 kHz, mono, 20 ms frames, VOIP application mode
- Bitrate 32 kbps (configurable 16–64 per room)
- In-band FEC on, expected loss 10%. DTX on. During silence the client sends
  nothing (VAD) or Opus DTX frames.

## Server rules

- Drop uplink audio if the sender lacks `speak` permission, is server-muted,
  or isn't in a room.
- `session_id` is allocated per room, monotonically (mod 2^16, skipping IDs
  in use). Don't reuse an ID quickly, so late packets aren't mapped to the
  wrong person.
- Rate limit: ≤ 60 datagrams/s per sender (20 ms frames = 50/s), payload
  ≤ 1000 bytes. Excess is dropped and counted.
- Top-N forwarding (rooms > N speakers): rank active senders by smoothed
  level (EMA over ~300 ms plus hysteresis) and forward the top N (default 5).
  **Caveat:** the client reports `level` itself, so a modified client can
  claim to be loud. Mitigations: cap trust per sender, optionally decode a
  sample of frames server-side to verify, moderators can mute.
- The server derives speaking state from `V`/`level` and sends debounced
  `SpeakingChanged` on the control stream, for clients that don't receive
  that sender's audio (top-N) or for UI-only consumers.

## Fallback when UDP is blocked

If QUIC can't connect within ~3 s, connect over TCP+TLS (WebSocket on 443,
same ALPN idea):

- Control messages: same framing in WebSocket binary messages tagged `0x00`
- Datagrams: same bytes in WebSocket binary messages tagged `0x01`

There's head-of-line blocking, but it works (Mumble does the same). The
client keeps retrying QUIC in the background.

## Future

- **E2EE:** encrypt the Opus payload with a per-room group key (MLS, like
  Discord's DAVE). Headers stay readable so the server can still forward and
  pick top-N. Conflicts with server-side recording/transcription.
- **Spatial audio:** optional position extension, either a flag bit plus
  trailing fields, or positions on the control stream at a lower rate (better).
- **Multiple rooms per connection:** new kinds with a room index byte.
