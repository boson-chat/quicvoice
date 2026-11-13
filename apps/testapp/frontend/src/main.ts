import {Events} from "@wailsio/runtime";
import {VoiceService} from "../bindings/github.com/boson-chat/quicvoice/apps/testapp";
import type {State, LogEntry, Participant} from "../bindings/github.com/boson-chat/quicvoice/apps/testapp/models";

const $ = <T extends HTMLElement = HTMLElement>(id: string) => document.getElementById(id) as T;

// --- persisted settings (per-machine conveniences only) ---

type Prefs = {
    addr: string; room: string; name: string; token: string; insecure: boolean;
    input: string; output: string; master: number; ptt: boolean;
};
const defaults: Prefs = {
    addr: "voice.boson.chat:7700", room: "test", name: "", token: "", insecure: false,
    input: "", output: "", master: 100, ptt: false,
};
function loadPrefs(): Prefs {
    try {
        return {...defaults, ...JSON.parse(localStorage.getItem("prefs") || "{}")};
    } catch {
        return {...defaults};
    }
}
const prefs = loadPrefs();
function savePrefs() {
    try {
        localStorage.setItem("prefs", JSON.stringify(prefs));
    } catch { /* private mode etc. */ }
}

// --- elements ---

const addr = $<HTMLInputElement>("addr");
const room = $<HTMLInputElement>("room");
const nameInput = $<HTMLInputElement>("name");
const token = $<HTMLTextAreaElement>("token");
const insecure = $<HTMLInputElement>("insecure");
const connectBtn = $<HTMLButtonElement>("connect");
const inputSel = $<HTMLSelectElement>("input");
const outputSel = $<HTMLSelectElement>("output");
const master = $<HTMLInputElement>("master");
const loopback = $<HTMLInputElement>("loopback");
const muteBtn = $<HTMLButtonElement>("mute");
const deafenBtn = $<HTMLButtonElement>("deafen");
const pttBtn = $<HTMLButtonElement>("ptt");
const people = $("people");

addr.value = prefs.addr;
room.value = prefs.room;
nameInput.value = prefs.name;
token.value = prefs.token;
insecure.checked = prefs.insecure;
master.value = String(prefs.master);
$("master-val").textContent = `${prefs.master}%`;

for (const [el, key] of [[addr, "addr"], [room, "room"], [nameInput, "name"], [token, "token"]] as const) {
    el.addEventListener("input", () => { (prefs as any)[key] = el.value.trim(); savePrefs(); });
}
insecure.addEventListener("change", () => { prefs.insecure = insecure.checked; savePrefs(); });

// --- token: mint locally when the app was started with JWT_SECRET ---

let canMint = false;
VoiceService.CanMintTokens().then(ok => {
    canMint = ok;
    $("mint-row").classList.toggle("hidden", !ok);
    $("token-row").classList.toggle("hidden", ok);
});

// --- connect ---

let state: State | null = null;

connectBtn.addEventListener("click", async () => {
    if (state?.connected) {
        await VoiceService.Disconnect();
        return;
    }
    connectBtn.disabled = true;
    try {
        let tok = token.value.trim();
        if (canMint) {
            tok = await VoiceService.MintToken(nameInput.value.trim());
        }
        await VoiceService.Connect({addr: addr.value.trim(), room: room.value.trim(), token: tok, insecure: insecure.checked});
    } catch (e: any) {
        addLog({time: now(), level: "error", message: String(e?.message ?? e)});
    } finally {
        connectBtn.disabled = false;
    }
});

// --- devices ---

async function loadDevices() {
    const d = await VoiceService.Devices();
    const fill = (sel: HTMLSelectElement, list: {id: string; name: string; isDefault: boolean}[], chosen: string) => {
        sel.replaceChildren(new Option("System default", ""));
        for (const dev of list ?? []) {
            sel.add(new Option(dev.name + (dev.isDefault ? " (default)" : ""), dev.id));
        }
        sel.value = list?.some(x => x.id === chosen) ? chosen : "";
    };
    fill(inputSel, d.inputs, prefs.input);
    fill(outputSel, d.outputs, prefs.output);
    if (prefs.input || prefs.output) {
        await VoiceService.SetDevices(inputSel.value, outputSel.value);
    }
}
for (const sel of [inputSel, outputSel]) {
    sel.addEventListener("change", () => {
        prefs.input = inputSel.value;
        prefs.output = outputSel.value;
        savePrefs();
        VoiceService.SetDevices(prefs.input, prefs.output);
    });
}
loadDevices();

master.addEventListener("input", () => {
    prefs.master = Number(master.value);
    $("master-val").textContent = `${prefs.master}%`;
    savePrefs();
    VoiceService.SetMasterVolume(prefs.master);
});
VoiceService.SetMasterVolume(prefs.master);

// --- mic mode, mute, deafen, PTT ---

function setMode(ptt: boolean) {
    prefs.ptt = ptt;
    savePrefs();
    $("mode-vad").classList.toggle("on", !ptt);
    $("mode-ptt").classList.toggle("on", ptt);
    pttBtn.classList.toggle("hidden", !ptt);
    VoiceService.SetPushToTalk(ptt);
}
$("mode-vad").addEventListener("click", () => setMode(false));
$("mode-ptt").addEventListener("click", () => setMode(true));
setMode(prefs.ptt);

loopback.addEventListener("change", () => VoiceService.SetLoopback(loopback.checked));
muteBtn.addEventListener("click", () => VoiceService.SetMuted(!state?.muted));
deafenBtn.addEventListener("click", () => VoiceService.SetDeafened(!state?.deafened));

let pttDown = false;
function ptt(down: boolean) {
    if (down === pttDown || !prefs.ptt) return;
    pttDown = down;
    pttBtn.classList.toggle("down", down);
    VoiceService.PTT(down);
}
pttBtn.addEventListener("pointerdown", () => ptt(true));
pttBtn.addEventListener("pointerup", () => ptt(false));
pttBtn.addEventListener("pointerleave", () => ptt(false));

const typing = () => ["INPUT", "TEXTAREA", "SELECT"].includes(document.activeElement?.tagName ?? "");
window.addEventListener("keydown", e => {
    if (typing()) return;
    if (e.code === "Space") { e.preventDefault(); ptt(true); }
    if (e.code === "KeyM" && !e.repeat) VoiceService.SetMuted(!state?.muted);
    if (e.code === "KeyD" && !e.repeat) VoiceService.SetDeafened(!state?.deafened);
});
window.addEventListener("keyup", e => { if (e.code === "Space") ptt(false); });
window.addEventListener("blur", () => ptt(false));

// --- per-user volume ---

people.addEventListener("input", e => {
    const t = e.target as HTMLInputElement;
    if (!t.dataset.sid) return;
    const pct = Number(t.value);
    VoiceService.SetUserVolume(Number(t.dataset.sid), pct);
    const out = t.parentElement?.querySelector("output");
    if (out) out.textContent = `${pct}%`;
});

// --- rendering ---

Events.On("voice:state", ev => render(ev.data as State));
Events.On("voice:log", ev => addLog(ev.data as LogEntry));

function render(s: State) {
    state = s;
    $("mic-fill").style.width = `${Math.round(s.micLevel * 100)}%`;
    $("tx").classList.toggle("on", s.transmitting && s.connected);
    muteBtn.classList.toggle("on", s.muted);
    muteBtn.textContent = s.muted ? "Unmute" : "Mute";
    deafenBtn.classList.toggle("on", s.deafened);
    deafenBtn.textContent = s.deafened ? "Undeafen" : "Deafen";
    loopback.checked = s.loopback;

    const err = $("audio-error");
    err.textContent = s.audioError ? `Audio device problem: ${s.audioError}` : "";
    err.classList.toggle("hidden", !s.audioError);

    const pill = $("status-pill");
    pill.classList.toggle("live", s.connected);
    pill.classList.toggle("busy", s.connecting);
    $("status-text").textContent = s.connected ? "connected" : s.connecting ? "connecting" : "offline";
    connectBtn.textContent = s.connected ? "Disconnect" : s.connecting ? "Connecting…" : "Connect";
    connectBtn.classList.toggle("danger", s.connected);
    for (const el of [addr, room, nameInput, token, insecure]) el.disabled = s.connected || s.connecting;

    $("room-title").textContent = s.connected ? `# ${s.room}` : "Not connected";
    $("status-line").textContent = s.connected
        ? `${s.addr} · ${s.participants.length} in room`
        : "Join a room to start talking.";
    $("stats").textContent = s.connected
        ? `rtt ${s.rttMs ? s.rttMs.toFixed(0) + " ms" : "–"} · sent ${s.framesSent} · recv ${s.framesRecv}`
        : "";

    renderPeople(s.connected ? s.participants : []);
}

const cards = new Map<number, HTMLElement>();

function renderPeople(list: Participant[]) {
    if (list.length === 0) {
        cards.clear();
        people.innerHTML = `<div class="empty">${state?.connected ? "Just you so far." : "No one here yet."}</div>`;
        return;
    }
    people.querySelector(".empty")?.remove();
    const seen = new Set<number>();
    for (const p of list) {
        seen.add(p.sessionId);
        let card = cards.get(p.sessionId);
        if (!card) {
            card = makeCard(p);
            cards.set(p.sessionId, card);
        }
        people.appendChild(card); // keeps server order; no-op if already in place
        updateCard(card, p);
    }
    for (const [sid, card] of cards) {
        if (!seen.has(sid)) { card.remove(); cards.delete(sid); }
    }
}

function makeCard(p: Participant): HTMLElement {
    const el = document.createElement("div");
    el.className = "person";
    el.innerHTML = `
      <div class="who">
        <div class="avatar"></div>
        <div style="min-width:0">
          <div class="name"></div>
          <div class="meta"></div>
        </div>
        <div class="badges"></div>
      </div>
      ${p.self ? "" : `
      <label class="vol">
        <input type="range" min="0" max="200" step="5" value="${p.volume}" data-sid="${p.sessionId}"/>
        <output>${p.volume}%</output>
      </label>
      <div class="quality"><span class="loss"></span><span class="buf"></span></div>`}
    `;
    el.querySelector(".avatar")!.textContent = (p.name || "?").slice(0, 1).toUpperCase();
    el.querySelector(".name")!.textContent = p.name + (p.self ? " (you)" : "");
    el.querySelector(".meta")!.textContent = `session ${p.sessionId} · ${p.role.toLowerCase()}`;
    return el;
}

function updateCard(el: HTMLElement, p: Participant) {
    el.classList.toggle("speaking", p.speaking);
    el.style.setProperty("--lvl", String(p.level));
    const badges: string[] = [];
    if (p.deafened) badges.push(`<span class="badge bad">deafened</span>`);
    else if (p.muted) badges.push(`<span class="badge bad">muted</span>`);
    el.querySelector(".badges")!.innerHTML = badges.join("");
    const loss = el.querySelector(".loss");
    if (loss) {
        loss.textContent = `loss ${p.lossPercent.toFixed(1)}%`;
        loss.className = "loss " + (p.lossPercent > 5 ? "bad" : p.lossPercent > 1 ? "warn" : "");
        el.querySelector(".buf")!.textContent = `buffer ${p.bufferMs} ms`;
    }
}

// --- log ---

const now = () => new Date().toTimeString().slice(0, 8);

function addLog(e: LogEntry) {
    const li = document.createElement("li");
    li.className = e.level;
    const t = document.createElement("time");
    t.textContent = e.time;
    li.append(t, e.message);
    const log = $("log");
    log.prepend(li);
    while (log.children.length > 200) log.lastElementChild?.remove();
    if (e.level === "error") (document.querySelector(".log") as HTMLDetailsElement).open = true;
}
