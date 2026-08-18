import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "./tests",
  timeout: 30_000,
  expect: { timeout: 10_000 },
  use: { baseURL: "http://127.0.0.1:18081", trace: "retain-on-failure" },
  projects: [
    { name: "desktop", browserName: "chromium", use: { ...devices["Desktop Chrome"], viewport: { width: 1280, height: 900 } } },
    // Keep Chromium explicitly: spreading iPhone 13 includes WebKit's
    // default browser type, which would silently stop exercising the same
    // engine as the desktop project.
    { name: "mobile", browserName: "chromium", use: { viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true, userAgent: devices["iPhone 13"].userAgent } }
  ],
  webServer: {
    command: "go run ./simulator/cmd/m261sim -modbus-addr=127.0.0.1:1502 -iec104-addr=127.0.0.1:12404 -control-addr=127.0.0.1:18081 -physics-step=1m -speed=60",
    cwd: "..",
    url: "http://127.0.0.1:18081/api/v1/health/live",
    reuseExistingServer: !process.env.CI,
    timeout: 30_000
  }
});
