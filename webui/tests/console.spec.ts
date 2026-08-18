import { expect, test } from "@playwright/test";

const dynamicValues = (page: import("@playwright/test").Page) => [
  page.locator(".topbar-status > span"),
  page.locator(".definition-list dd"),
];

test("matches the approved MVP screens and confirmation state", async ({
  page,
}) => {
  await page.goto("/");
  await expect(page.getByText("SIMULATOR", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "Overview / Demo" }),
  ).toBeVisible();
  await expect(page.getByText("Unconfirmed").first()).toBeVisible();
  await expect(page).toHaveScreenshot("overview.png", {
    fullPage: true,
    mask: dynamicValues(page),
    maxDiffPixels: 100,
  });

  await page.getByRole("button", { name: "Control" }).click();
  await expect(page.getByRole("heading", { name: "Control" })).toBeVisible();
  await expect(page).toHaveScreenshot("control.png", {
    fullPage: true,
    mask: dynamicValues(page),
    maxDiffPixels: 100,
  });
  await page.getByRole("button", { name: "Trip…" }).click();
  await expect(
    page.getByRole("dialog", { name: "Confirm Trip" }),
  ).toBeVisible();
  await expect(page).toHaveScreenshot("trip-confirmation.png", {
    fullPage: true,
    mask: dynamicValues(page),
    maxDiffPixels: 100,
  });
  await page.getByRole("button", { name: "Cancel" }).click();

  await page.getByRole("button", { name: "Test Lab" }).click();
  await expect(page.getByRole("heading", { name: "Test Lab" })).toBeVisible();
  await expect(page.getByText("Alarm injection")).toBeVisible();
  await expect(page).toHaveScreenshot("test-lab.png", {
    fullPage: true,
    mask: dynamicValues(page),
    maxDiffPixels: 100,
  });
});

test("surfaces accepted-but-unsupported danger diagnostics", async ({
  page,
}) => {
  await page.route("/api/v1/commands", async (route) => {
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify({
        device: "EMS",
        slug: "trip",
        accepted_value: 1,
        readback: 1,
        diagnostic: {
          code: "accepted_but_unsupported",
          point_key: { Device: "EMS", Slug: "trip" },
          accepted_value: 1,
        },
      }),
    });
  });
  await page.goto("/");
  await page.getByRole("button", { name: "Control" }).click();
  await page.getByRole("button", { name: "Trip…" }).click();
  await page.getByRole("button", { name: "Confirm action" }).click();
  await expect(
    page.getByText("Trip was accepted, but has no modeled physical effect."),
  ).toBeVisible();
});

test("switches every MVP screen to Ukrainian", async ({ page }) => {
  await page.goto("/");
  await page.locator(".language-switch").click();
  await expect(
    page.getByRole("heading", { name: "Огляд / Демонстрація" }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Підготувати демонстрацію" }).click();
  await expect(
    page.getByText(
      "Середовище демонстрації підготовлено та підтверджено симулятором.",
    ),
  ).toBeVisible();

  await page.getByRole("button", { name: "Керування" }).click();
  await expect(page.getByText("Небезпечна зона")).toBeVisible();
  await expect(page.getByText("Робочий стан")).toBeVisible();

  await page.getByRole("button", { name: "Стенд" }).click();
  await expect(page.getByText("Активація аварії")).toBeVisible();
});

test("keeps reset confirmation focus inside the dialog and cancels on Escape", async ({ page }) => {
  await page.goto("/");
  await page.getByRole("button", { name: "Test Lab" }).click();
  await page.getByRole("button", { name: "Reset…" }).click();
  const dialog = page.getByRole("dialog", { name: "Confirm reset" });
  await expect(dialog).toBeVisible();
  await expect(page.getByRole("button", { name: "Cancel" })).toBeFocused();
  for (let index = 0; index < 5; index++) {
    await page.keyboard.press("Tab");
    await expect(dialog.locator(":focus")).toBeVisible();
  }
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
});

test("keeps the connection status visible on mobile", async ({ page }, testInfo) => {
  test.skip(testInfo.project.name !== "mobile", "This is a mobile breakpoint regression test.");
  await page.goto("/");
  await expect(page.locator(".topbar-status .chip")).toBeVisible();
});

// Task 10.1 item 5: the About dialog's numbers must come from the live
// catalog, never a hardcoded literal -- fetch the same catalog the UI
// fetches and assert the displayed counts match it exactly, so this test
// would fail if the UI ever drifted to a stale hardcoded number instead.
test("About dialog is accessible, catalog-sourced, and screenshot-covered", async ({
  page,
}) => {
  await page.goto("/");
  const catalogResponse = await page.request.get("/api/v1/catalog");
  expect(catalogResponse.ok()).toBeTruthy();
  const catalog = (await catalogResponse.json()) as {
    points: Array<{ device: string }>;
  };
  const expectedPointCount = catalog.points.length;
  const expectedDeviceCount = new Set(catalog.points.map((p) => p.device))
    .size;

  await page.getByRole("button", { name: "About", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "About this program" });
  await expect(dialog).toBeVisible();
  await expect(page.getByRole("button", { name: "Close" })).toBeFocused();

  await expect(
    dialog.getByText(`${expectedPointCount} modeled data points`),
  ).toBeVisible();
  await expect(
    dialog.getByText(`${expectedDeviceCount} simulated devices`),
  ).toBeVisible();

  await expect(page).toHaveScreenshot("about-dialog.png", {
    fullPage: true,
    mask: dynamicValues(page),
    maxDiffPixels: 100,
  });

  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
});

test("heartbeat indicator moves from awaiting to live once a tick is observed", async ({
  page,
}) => {
  await page.goto("/");
  const heartbeat = page.locator(".heartbeat");
  // Always rendered, in the same shape, regardless of tick timing --
  // otherwise this element's presence/absence would itself be a source of
  // flaky, timing-dependent layout shifts (see Task 10.1 fix notes).
  await expect(heartbeat).toBeVisible();
  await expect(heartbeat).toContainText("Awaiting first tick");
  await expect(heartbeat).toContainText("Heartbeat live", { timeout: 10_000 });
});

test("SoC and power chart shows two real series, not one", async ({
  page,
}) => {
  await page.goto("/");
  const chart = page.locator(".dashboard-grid .chart-card").first();
  const img = chart.locator("[role='img']");
  await expect(img).toHaveAttribute("aria-label", /State of charge/);
  await expect(img).toHaveAttribute("aria-label", /Actual power/);
});

// Task 10.1 item 1: the progress card must not exist before any step has
// actually run (never predict steps that haven't happened yet), then must
// show exactly the steps a real scenario run produced, in order.
test("shows scenario progress and recent events only as they actually happen", async ({
  page,
}) => {
  await page.goto("/");
  await expect(
    page.getByRole("heading", { name: "Overview / Demo" }),
  ).toBeVisible();
  await expect(page.getByText("Scenario progress")).toHaveCount(0);

  await post(page, "/scenario/load", { name: "72_hour_monitoring.yaml" });
  await post(page, "/scenario/start");
  await expect
    .poll(() => scenarioStatus(page), { timeout: 60_000 })
    .toMatchObject({ running: false, cursor: 3, error: "" });

  await expect(
    page.getByRole("heading", { name: "Scenario progress" }),
  ).toBeVisible();
  await expect(page.locator(".scenario-step")).toHaveCount(3);

  await expect(
    page.getByRole("heading", { name: "Recent events" }),
  ).toBeVisible();
  await expect(page.locator(".recent-event").first()).toBeVisible();
});

type State = { points: Array<{ device: string; slug: string; value: number }> };
type ScenarioStatus = { running: boolean; cursor: number; error: string };

async function post(
  page: import("@playwright/test").Page,
  path: string,
  data?: unknown,
) {
  const response = await page.request.post(path, { data });
  expect(response.ok(), `${path}: ${await response.text()}`).toBeTruthy();
}

async function state(page: import("@playwright/test").Page) {
  const response = await page.request.get("/api/v1/state");
  expect(response.ok()).toBeTruthy();
  return response.json() as Promise<State>;
}

async function scenarioStatus(page: import("@playwright/test").Page) {
  const response = await page.request.get("/api/v1/scenario/status");
  expect(response.ok()).toBeTruthy();
  return response.json() as Promise<ScenarioStatus>;
}

async function prepareFromUI(page: import("@playwright/test").Page) {
  await Promise.all([
    page.waitForResponse(
      (response) =>
        response.url().endsWith("/api/v1/demo/prepare") &&
        response.status() === 200,
    ),
    page.getByRole("button", { name: "Prepare Demo" }).click(),
  ]);
  await expect(
    page.getByText("Demo environment prepared and confirmed by the simulator."),
  ).toBeVisible();
}

test("runs the deterministic guided demo ten times from the browser", async ({
  page,
}, testInfo) => {
  test.skip(
    testInfo.project.name !== "desktop",
    "The demo is exercised once in Chromium; visual coverage runs at both approved widths.",
  );
  test.setTimeout(240_000);
  await page.goto("/");
  const baselines: Array<{ soc: number; desired: number; cursor: number }> = [];

  for (let run = 0; run < 10; run++) {
    await prepareFromUI(page);
    const prepared = await state(page);
    const value = (device: string, slug: string) =>
      prepared.points.find(
        (point) => point.device === device && point.slug === slug,
      )?.value;
    expect(value("BMS", "soc")).toBe(50);

    // Remote -100 kW, then a model-time advance, power limiting, and the
    // alarm/link actions all use the same HTTP endpoints the UI invokes.
    await post(page, "/api/v1/commands", {
      device: "EMS",
      slug: "set_operating_mode",
      value: 2,
    });
    await post(page, "/api/v1/commands", {
      device: "EMS",
      slug: "set_active_power_kw",
      value: -100,
    });
    await post(page, "/clock/advance", { by_seconds: 60 });
    await post(page, "/api/v1/commands", {
      device: "EMS",
      slug: "system_maximum_charge_power",
      value: 50,
    });
    await post(page, "/faults", {
      device: "BMS",
      point: "cell_temperature_too_high",
      value: 1,
    });
    await post(page, "/link", {
      protocol: "modbus",
      mode: "drop",
      delay_ms: 0,
    });
    await post(page, "/link/clear", { protocol: "modbus" });

    await post(page, "/scenario/load", { name: "72_hour_monitoring.yaml" });
    await post(page, "/scenario/start");
    await expect
      .poll(() => scenarioStatus(page), { timeout: 60_000 })
      .toMatchObject({ running: false, cursor: 3, error: "" });
    const completed = await state(page);
    const desired = completed.points.find(
      (point) =>
        point.device === "EMS" && point.slug === "desired_active_power_kw",
    )?.value;
    const completeStatus = await scenarioStatus(page);
    baselines.push({
      soc: value("BMS", "soc") ?? Number.NaN,
      desired: desired ?? Number.NaN,
      cursor: completeStatus.cursor,
    });

    await page.getByRole("button", { name: "Test Lab" }).click();
    await page.getByRole("button", { name: "Reset…" }).click();
    await expect(
      page.getByRole("dialog", { name: "Confirm reset" }),
    ).toBeVisible();
    await Promise.all([
      page.waitForResponse(
        (response) =>
          response.url().endsWith("/reset") && response.status() === 204,
      ),
      page.getByRole("button", { name: "Confirm action" }).click(),
    ]);
    await page.getByRole("button", { name: "Overview / Demo" }).click();
  }
  expect(
    new Set(baselines.map((baseline) => JSON.stringify(baseline))).size,
  ).toBe(1);
});
