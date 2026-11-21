// Test page for the browser client: exposes window.qv for Playwright.
// ?api=http://host:port (the public API) — /v1/info gives the QUIC port
// and, for a dev certificate, its hash.
import { QuicVoiceClient, support } from "./client";
import type { Downlink } from "./wire";

const log = (s: string) => {
  document.getElementById("log")!.textContent += s + "\n";
};

interface Info {
  quic_port: number;
  webtransport_path: string;
  dev_cert_hash?: string;
}

async function info(api: string): Promise<Info> {
  return (await fetch(`${api}/v1/info`)).json();
}

/** GET /v1/rooms with a bearer token: CORS (preflight) from this origin. */
async function rooms(api: string, token: string): Promise<string[]> {
  const r = await fetch(`${api}/v1/rooms`, { headers: { Authorization: `Bearer ${token}` } });
  if (!r.ok) throw new Error(`rooms: ${r.status}`);
  const body = (await r.json()) as { rooms: { id: string }[] };
  return body.rooms.map((x) => x.id);
}

async function connect(api: string, token: string): Promise<QuicVoiceClient> {
  const i = await info(api);
  const host = new URL(api).hostname;
  return QuicVoiceClient.connect(`https://${host}:${i.quic_port}${i.webtransport_path}`, token, { certHash: i.dev_cert_hash });
}

/** 20 ms of a 440 Hz tone at 48 kHz, starting at sample n. */
function tone(n: number): Float32Array<ArrayBuffer> {
  const f = new Float32Array(960);
  for (let i = 0; i < f.length; i++) f[i] = 0.3 * Math.sin((2 * Math.PI * 440 * (n + i)) / 48000);
  return f;
}

/**
 * Alice and Bob join `room`; Alice sends `frames` datagrams (raw bytes, or
 * real Opus from AudioEncoder when opus is set) and Bob counts what arrives
 * (and decodes it with AudioDecoder).
 */
async function exchange(api: string, room: string, frames: number, opus: boolean) {
  const alice = await connect(api, `alice-${room}`);
  const bob = await connect(api, `bob-${room}`);
  const as = await alice.join(room);
  const bs = await bob.join(room);
  const got: Downlink[] = [];
  bob.on("audio", (d) => got.push(d));

  let decoded = 0;
  let energy = 0;
  let decoder: AudioDecoder | null = null;
  if (opus) {
    decoder = new AudioDecoder({
      output: (a) => {
        const buf = new Float32Array(a.numberOfFrames);
        a.copyTo(buf, { planeIndex: 0, format: "f32-planar" });
        for (const v of buf) energy += v * v;
        decoded++;
        a.close();
      },
      error: (e) => log(`decoder: ${e}`),
    });
    decoder.configure({ codec: "opus", sampleRate: 48000, numberOfChannels: 1 });
    bob.on("audio", (d) => decoder!.decode(new EncodedAudioChunk({ type: "key", timestamp: d.timestamp * 1e6 / 48000, data: d.payload })));
  }

  const payloads: Uint8Array[] = [];
  if (opus) {
    const enc = new AudioEncoder({
      output: (c) => {
        const b = new Uint8Array(c.byteLength);
        c.copyTo(b);
        payloads.push(b);
      },
      error: (e) => log(`encoder: ${e}`),
    });
    enc.configure({ codec: "opus", sampleRate: 48000, numberOfChannels: 1, bitrate: 32000 });
    for (let i = 0; i < frames; i++) {
      enc.encode(new AudioData({ format: "f32", sampleRate: 48000, numberOfChannels: 1, numberOfFrames: 960, timestamp: i * 20000, data: tone(i * 960) }));
    }
    await enc.flush();
    enc.close();
  } else {
    for (let i = 0; i < frames; i++) payloads.push(new TextEncoder().encode(`frame-${i}`));
  }
  let ts = 0;
  for (let i = 0; i < payloads.length; i++) {
    alice.sendAudio({ flags: i === 0 ? 1 : 0, voice: true, level: 20, seq: i, timestamp: ts, payload: payloads[i] });
    ts += 960;
    await new Promise((r) => setTimeout(r, 10));
  }
  const end = Date.now() + 3000;
  while (got.length < payloads.length * 0.8 && Date.now() < end) await new Promise((r) => setTimeout(r, 50));
  if (decoder) {
    await decoder.flush().catch(() => {});
    decoder.close();
  }
  const out = {
    sent: payloads.length,
    received: got.length,
    fromAlice: got.every((d) => d.sessionId === as.yourSessionId),
    participants: bs.participants.length,
    maxPayload: alice.welcome.maxDatagramPayload,
    firstPayload: opus ? "" : new TextDecoder().decode(got[0]?.payload ?? new Uint8Array()),
    decoded,
    rms: decoded ? Math.sqrt(energy / (decoded * 960)) : 0,
    rttMs: alice.rttMs,
  };
  alice.close();
  bob.close();
  log(JSON.stringify(out));
  return out;
}

/** A bad token: the server's fatal UNAUTHENTICATED error. */
async function badToken(api: string): Promise<{ code: number; message: string; fatal: boolean }> {
  try {
    (await connect(api, "")).close();
    return { code: 0, message: "accepted", fatal: false };
  } catch (e) {
    const err = e as { code?: number; message: string; fatal?: boolean };
    return { code: err.code ?? -1, message: err.message, fatal: !!err.fatal };
  }
}

const qv = { support, info, rooms, exchange, badToken };
(globalThis as unknown as { qv: typeof qv }).qv = qv;
void support().then((s) => {
  document.getElementById("support")!.textContent = s.missing.length
    ? `This browser can't do voice: no ${s.missing.join(", ")}.`
    : "WebTransport + Opus: supported.";
});
