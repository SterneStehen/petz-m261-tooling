import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "./tests",
  timeout: 30_000,
  expect: { timeout: 10_000 },
  use: { baseURL: "http://127.0.0.1:18081", trace: "retain-on-failure" },
  projects: [
    { name: "desktop", use: { browserName: "chromium", ...devices["Desktop Chrome"], viewport: { width: 1280, height: 900 } } },
    { name: "mobile", use: { browserName: "chromium", ...devices["iPhone 13"] } }
  ],
  webServer: {
    command: "go run ./simulator/cmd/m261sim -modbus-addr=127.0.0.1:1502 -iec104-addr=127.0.0.1:12404 -control-addr=127.0.0.1:18081",
    cwd: "..",
    url: "http://127.0.0.1:18081/api/v1/health/live",
    reuseExistingServer: !process.env.CI,
    timeout: 30_000
  }
});
