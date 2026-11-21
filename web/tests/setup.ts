// Starts quicvoiced (AUTH_INSECURE, a 13-day self-signed dev certificate,
// WebTransport on its QUIC port) and serves the test page from another
// origin, so the page's API calls go through CORS like a real web app's.
import { execFileSync, spawn } from "node:child_process";
import { mkdirSync, readFileSync } from "node:fs";
import { createServer } from "node:http";
import path from "node:path";

export const QUIC_PORT = 17731;
export const API_PORT = 18391;
export const PAGE_PORT = 18392;
export const API = `http://localhost:${API_PORT}`;
const repo = path.resolve(path.dirname(new URL(import.meta.url).pathname), "../..");
const dist = path.join(repo, "web/dist");

async function waitFor(url: string, ms: number): Promise<void> {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    try {
      if ((await fetch(url)).ok) return;
    } catch {}
    await new Promise((r) => setTimeout(r, 200));
  }
  throw new Error(`${url} not up`);
}

export default async function globalSetup(): Promise<() => void> {
  const bin = path.join(repo, "web/test-results/bin");
  mkdirSync(bin, { recursive: true });
  execFileSync("go", ["build", "-tags", "nolibopusfile", "-o", path.join(bin, "quicvoiced"), "./server/cmd"], { cwd: repo, stdio: "inherit" });
  execFileSync("npm", ["run", "build"], { cwd: path.join(repo, "web"), stdio: "inherit" });
  const server = spawn(path.join(bin, "quicvoiced"), ["serve"], {
    env: {
      ...process.env, AUTH_INSECURE: "true", ENV: "production", VOICE_ROOMS_STRICT: "false",
      QUIC_ADDR: `:${QUIC_PORT}`, HTTP_ADDR: "127.0.0.1:18381", PUBLIC_HTTP_ADDR: `127.0.0.1:${API_PORT}`,
      WEBTRANSPORT_ORIGINS: `http://localhost:${PAGE_PORT}`,
    },
    stdio: "inherit",
  });
  const types: Record<string, string> = { ".html": "text/html", ".js": "text/javascript" };
  const page = createServer((req, res) => {
    const name = (req.url ?? "/").split("?")[0] === "/" ? "index.html" : path.basename(req.url!.split("?")[0]);
    try {
      const body = readFileSync(path.join(dist, name));
      res.writeHead(200, { "Content-Type": types[path.extname(name)] ?? "application/octet-stream" });
      res.end(body);
    } catch {
      res.writeHead(404).end();
    }
  }).listen(PAGE_PORT, "127.0.0.1");
  await waitFor(`${API}/healthz`, 15_000);
  return () => {
    page.close();
    server.kill();
  };
}
