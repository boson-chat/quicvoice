import { expect, type Page, test } from "@playwright/test";
import { API, PAGE_PORT } from "./setup";

interface Support {
  webTransport: boolean;
  opus: boolean;
  missing: string[];
}

async function open(page: Page): Promise<Support> {
  await page.goto(`http://localhost:${PAGE_PORT}/`);
  await page.waitForFunction(() => "qv" in globalThis);
  return page.evaluate(() => (globalThis as any).qv.support());
}

test("the public API answers this page's origin (CORS, /v1/info)", async ({ page }) => {
  await open(page);
  const info = await page.evaluate((api) => (globalThis as any).qv.info(api), API);
  expect(info.webtransport_path).toBe("/wt");
  expect(info.dev_cert_hash).toMatch(/^[0-9a-f]{64}$/);
  const rooms = await page.evaluate((api) => (globalThis as any).qv.rooms(api, "someone"), API);
  expect(rooms).toContain("general");
});

test("browsers without WebTransport say so", async ({ page }, info) => {
  const s = await open(page);
  console.log(`${info.project.name}: ${JSON.stringify(s)}`);
  await expect(page.locator("#support")).toHaveText(
    s.missing.length ? new RegExp(`can't do voice: no ${s.missing.join(", ")}`) : "WebTransport + Opus: supported.",
  );
});

test("datagrams flow between two WebTransport sessions", async ({ page }, info) => {
  const s = await open(page);
  test.skip(!s.webTransport, `${info.project.name} has no WebTransport`);
  const r = await page.evaluate((api) => (globalThis as any).qv.exchange(api, `raw-${Date.now()}`, 40, false), API);
  expect(r.participants).toBe(2);
  expect(r.maxPayload).toBe(1000);
  expect(r.received).toBeGreaterThanOrEqual(32);
  expect(r.fromAlice).toBe(true);
  expect(r.firstPayload).toMatch(/^frame-\d+$/);
});

test("real Opus from AudioEncoder decodes on the other side", async ({ page }, info) => {
  const s = await open(page);
  test.skip(!s.webTransport || !s.opus, `${info.project.name} lacks ${s.missing.join(", ")}`);
  const r = await page.evaluate((api) => (globalThis as any).qv.exchange(api, `opus-${Date.now()}`, 50, true), API);
  expect(r.received).toBeGreaterThanOrEqual(40);
  expect(r.decoded).toBeGreaterThanOrEqual(40);
  // The 440 Hz tone at 0.3 amplitude has an RMS of ~0.21.
  expect(r.rms).toBeGreaterThan(0.1);
});

test("a bad token is a fatal UNAUTHENTICATED error", async ({ page }, info) => {
  const s = await open(page);
  test.skip(!s.webTransport, `${info.project.name} has no WebTransport`);
  const r = await page.evaluate((api) => (globalThis as any).qv.badToken(api), API);
  expect(r).toMatchObject({ code: 1, fatal: true });
});
