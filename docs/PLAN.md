# Plan

## Goals

- Voice rooms for a Wails app: desktop (macOS, Windows, Linux) and mobile
  (iOS, Android, via Wails v3 mobile, which is still experimental).
- Go everywhere: server + Go client SDK. The UI only talks to the SDK through
  Wails bindings/events and never touches audio.
- Per-user volume, speaking indicators, mute/PTT/VAD, moderation, solid
  reconnects.

Non-goals for v1: browser clients, P2P, video, recording, E2EE.

### Mobile

Wails v3 compiles the Go side into the mobile app (a static lib on iOS,
`libwails.so` via the NDK on Android), so the same client SDK, quic-go and
cgo libs (opus, miniaudio) run on mobile. Audio stays in Go and never goes
through the webview. iOS suspends WKWebView media in the background.

What differs per platform:

| | iOS | Android |
|---|---|---|
| AEC/NS/AGC | OS voice processing: AVAudioSession `.playAndRecord` + mode `.voiceChat` / VoiceProcessingIO unit (small cgo/Obj-C shim if miniaudio's RemoteIO path can't enable it) | OS voice processing: AAudio input preset `VOICE_COMMUNICATION` + usage `VOICE_COMMUNICATION` (miniaudio exposes both); verify per device and fall back to webrtc-audio-processing |
| Background audio | `UIBackgroundModes: audio` (+ `voip`) | Foreground service, type `microphone` + ongoing notification |
| System call UI | CallKit (lock screen, interruptions) — later | ConnectionService / Telecom — later |
| Interruptions | Phone calls, Siri, other apps → pause capture, rejoin | Audio focus loss → same |
| Routing | Earpiece vs speaker, Bluetooth HFP vs A2DP | Same, plus `MODE_IN_COMMUNICATION` |
| Permissions | `NSMicrophoneUsageDescription` | `RECORD_AUDIO`, `FOREGROUND_SERVICE_MICROPHONE`, `POST_NOTIFICATIONS` |

The native bits (audio session, foreground service, CallKit) need a small
native plugin layer next to the Go SDK, exposed as `client/platform` hooks.
QUIC connection migration covers wifi ↔ cellular switching.

## Architecture

```
┌──────────── Wails app ─────────────┐          ┌────────── server ──────────┐
│ webview UI  ⇄  bindings/events     │          │ auth (JWT)                 │
│                  │                 │  QUIC    │ room manager               │
│            client SDK (Go)  ───────┼─────────►│ forwarder (datagram fanout)│
│  malgo ⇄ DSP ⇄ opus ⇄ jitter/mix   │ ctrl+dgrams top-N / speaking       │
└────────────────────────────────────┘          │ webhooks, metrics          │
                                                └────────────────────────────┘
```

### Client audio pipeline

```
capture:  malgo (48k mono, 10ms period) → AEC/NS/AGC → VAD | PTT gate
          → level meter → Opus encode (20ms) → wire.Uplink → QUIC datagram

playback: QUIC datagram → wire.Downlink → per-sender jitter buffer
          → Opus decode (PLC/FEC on gaps) → per-user gain → mix
          → master gain → limiter → malgo playback (also the AEC far-end reference)
```

Volume management lives in the client mixer:

- Per-user gain (0–200%), persisted by user_id in the app (the SDK just
  exposes `SetUserVolume`).
- Optional loudness normalization per sender (slow AGC on the receive side).
- Soft limiter on the final mix so it doesn't clip with many talkers.
- Optional ducking: lower others while a priority speaker talks.

## Repo layout (target)

```
/proto            control.proto → gen/controlpb
/wire             datagram headers (done: sketch + tests)
/server
  /cmd/quicvoiced
  /auth  /room  /forward  /webhook  /metrics
/client           Go SDK: Connect, Join, Mute, SetUserVolume, events
  /audio          malgo device I/O
  /dsp            AEC/NS/AGC (cgo), VAD, level meter, limiter
  /codec          opus (cgo)
  /jitter         adaptive jitter buffer
  /mix            mixer
/examples
  /bot            headless client (plays a wav, records a room); also used for load tests
  /wails          minimal Wails demo
```

## Milestones

| # | Milestone | Done when | Est. |
|---|---|---|---|
| 0 | **Spike** | Two Go CLIs talk through a dumb QUIC forwarder (malgo + opus, no DSP). Measure latency. Confirm cgo builds on all 3 OSes | 3–4 d |
| 1 | **Protocol + server core** | Control stream, JWT auth, rooms, session IDs, datagram fanout, rate limits, permissions. Bot client in tests | 1–1.5 wk |
| 2 | **Client SDK core** | Connect/reconnect state machine, join/leave, events, jitter buffer, mixer, per-user volume, limiter | 1.5–2 wk |
| 3 | **Audio quality** | AEC + NS + AGC via cgo (webrtc-audio-processing), VAD, PTT, device selection + hotplug, mic test/loopback | 1.5–2 wk |
| 4 | **Room features** | Server-derived speaking events, mute/deafen, moderation, roles, voice targets (whisper), Data messages | 1 wk |
| 5 | **Scale + ops** | Top-N forwarding, connection quality, metrics (VictoriaMetrics/OTel), webhooks, WS fallback, load test with 200 bots | 1–1.5 wk |
| 6 | **Wails integration** | Demo app, packaging + signing, CI builds for mac/win/linux | 1 wk |
| 7 | **Mobile** | iOS + Android builds of the demo, OS voice processing, background audio, interruptions, audio routing, permissions, testing on real devices | 3–5 wk |
| 8 | **Mobile polish (later)** | CallKit / ConnectionService, push to invite into a room, battery tuning | 2–3 wk |

**Total ≈ 11–15 weeks** for desktop + mobile v1 (M0–M7). M0 is there to surface the big risks
(cgo cross-builds, AEC, Wails mobile maturity) early. **M0 should include a
throwaway iOS + Android build** that captures the mic and plays it back through
the Go side, to prove the Wails mobile + miniaudio + opus toolchain before
committing.

## Libraries

| Need | Choice |
|---|---|
| QUIC | `github.com/quic-go/quic-go` (`EnableDatagrams: true`) |
| Audio I/O | `github.com/gen2brain/malgo` |
| Opus | `github.com/hraban/opus` (cgo, libopus) |
| AEC/NS/AGC | iOS/macOS/Android: OS voice processing. Windows/Linux: `webrtc-audio-processing` (freedesktop standalone build) via a thin cgo wrapper. Fallback: speexdsp. All behind one `dsp.Processor` interface |
| Noise suppression (alt) | RNNoise via cgo |
| Control encoding | protobuf (`google.golang.org/protobuf`) |
| Auth | `github.com/golang-jwt/jwt/v5` |
| WS fallback | `github.com/coder/websocket` |
| Metrics | OTel → VictoriaMetrics |

## Risks

1. **AEC quality + cgo builds.** The biggest risk. Wrap it behind an
   interface so the DSP backend can be swapped (OS voice processing on macOS,
   speexdsp, none + "use headphones").
2. **Cross-platform audio quirks:** Bluetooth headsets switching profiles,
   device hotplug, Windows exclusive mode. Budget time for testing on real
   hardware.
3. **Jitter buffer tuning:** start simple (fixed 60 ms target + PLC), then
   make it adaptive from measured jitter.
4. **UDP blocked networks:** WS fallback (M5). Measure how often it's needed.
5. **Level spoofing for top-N:** see PROTOCOL.md. Low priority for v1.
6. **Wails mobile is experimental.** APIs and build tooling may change. The
   SDK must not depend on Wails: it stays a plain Go module with a thin Wails
   binding package. If Wails mobile falls through, the same SDK can ship
   through gomobile bindings inside a native or Flutter shell.
7. **Mobile OS behaviour:** background limits, audio interruptions, Bluetooth
   quirks and device-specific Android echo cancellation. Only real-device
   testing catches these.

## Open questions

- Room lifecycle: does the server create rooms when someone joins, or does
  the app backend create them via an admin API?
- Max room size target? It decides whether top-N is in v1.
- Where do per-user volume prefs live: local only, or synced through the app
  backend?
- One room per connection is enough for v1? (Discord model)
- Is a browser client ever wanted? If so, plan a WebRTC↔quicvoice gateway on
  the server; the Opus frames pass straight through.
- Project name.
