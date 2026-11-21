import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "tests",
  timeout: 60_000,
  globalSetup: "./tests/setup.ts",
  workers: 1,
  projects: [
    // Full Chromium (not the headless shell) for WebCodecs + WebTransport.
    { name: "chromium", use: { channel: "chromium" } },
    { name: "firefox", use: { ...devices["Desktop Firefox"] } },
    { name: "webkit", use: { ...devices["Desktop Safari"] } },
  ],
  reporter: [["list"]],
});
