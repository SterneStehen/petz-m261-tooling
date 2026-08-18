import { expect, test } from "@playwright/test";

test("renders the approved MVP screens and a dangerous-action confirmation", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByText("SIMULATOR")).toBeVisible();
  await expect(page.getByRole("heading", { name: "Overview / Demo" })).toBeVisible();
  await expect(page.getByText("Unconfirmed").first()).toBeVisible();

  await page.getByRole("button", { name: "Control" }).click();
  await expect(page.getByRole("heading", { name: "Control" })).toBeVisible();
  await page.getByRole("button", { name: "Trip…" }).click();
  await expect(page.getByRole("dialog", { name: "Confirm Trip" })).toBeVisible();
  await page.getByRole("button", { name: "Cancel" }).click();

  await page.getByRole("button", { name: "Test Lab" }).click();
  await expect(page.getByRole("heading", { name: "Test Lab" })).toBeVisible();
  await expect(page.getByText("Alarm injection")).toBeVisible();
});
