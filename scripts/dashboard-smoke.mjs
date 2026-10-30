// Run scripts/preview first; this only inspects the disposable synthetic server.
import { chromium } from "playwright-core";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
const browser = await chromium.launch({
  executablePath: process.env.CHROMIUM_PATH || "/usr/bin/chromium",
  headless: true,
  args: ["--no-sandbox"],
});
const context = await browser.newContext({
  httpCredentials: { username: "preview", password: "preview" },
  viewport: { width: 1600, height: 1100 },
  colorScheme: "dark",
});
const errors = [];
const page = await context.newPage();
page.on("pageerror", (error) => errors.push(error.message));
const base = process.env.DASHBOARD_PREVIEW_URL || "http://127.0.0.1:8081";
const artifacts =
  process.env.DASHBOARD_ARTIFACTS || "/tmp/hems-dashboard-artifacts";
await fs.mkdir(artifacts, { recursive: true });
try {
  await page.goto(base + "/overview?sender=1&range=24h");
  await page.locator("#dashboard-content:not([hidden])").waitFor();
  assert.equal(await page.locator(".kpi").count(), 6);
  assert.ok(
    parseInt(await page.locator("#dataset-count").innerText(), 10) >= 90,
  );
  assert.equal(await page.locator("#energy-chart canvas").count(), 1);
  assert.equal(
    (await page.locator('nav a[aria-current="page"]').innerText()).replace(
      /\s+/g,
      " ",
    ),
    "⌂ Overview",
  );
  await page.screenshot({
    path: artifacts + "/overview-dark.png",
    fullPage: true,
  });
  await page.locator("#theme-toggle").click();
  assert.equal(await page.locator("html").getAttribute("data-theme"), "light");
  await page.screenshot({
    path: artifacts + "/overview-light.png",
    fullPage: true,
  });
  await page.reload();
  assert.equal(await page.locator("html").getAttribute("data-theme"), "light");
  await page.locator("#dashboard-content:not([hidden])").waitFor();
  await page.locator("#range").selectOption("6h");
  await page.waitForURL(/range=6h/);
  await page.waitForFunction(
    () =>
      parseInt(document.querySelector("#dataset-count").textContent, 10) >=
        20 &&
      parseInt(document.querySelector("#dataset-count").textContent, 10) < 40,
  );
  await page.goBack();
  await page.waitForFunction(
    () => document.querySelector("#range").value === "24h",
  );
  for (const route of [
    "hems",
    "runtime",
    "devices",
    "live",
    "uploads",
    "sources",
    "downloads",
    "uploads/96",
  ]) {
    await page.goto(base + "/" + route + "?sender=1&range=24h");
    if (["hems", "runtime", "devices", "live"].includes(route)) {
      await page.locator("#dashboard-content:not([hidden])").waitFor();
    }
    if (route === "hems")
      assert.equal(await page.locator("#schedule tr").count(), 6);
    if (route === "live")
      assert.match(
        await page.locator("#dataset-count").innerText(),
        /^1 snapshots/,
      );
    if (["hems", "runtime", "devices"].includes(route)) {
      await page.screenshot({
        path: `${artifacts}/${route}.png`,
        fullPage: true,
      });
    }
  }
  await page.goto(base + "/overview?sender=1&range=24h");
  await page.locator("#dashboard-content:not([hidden])").waitFor();
  await page.locator("#theme-toggle").click();
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: artifacts + "/overview-mobile.png",
    fullPage: true,
  });
  assert.equal(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
    true,
  );
  await page.locator("#range").selectOption("custom");
  await page.locator("#from").fill("2020-01-01T00:00");
  await page.locator("#to").fill("2020-01-02T00:00");
  await page.locator('#dashboard-filters button[type="submit"]').click();
  await page.locator("#empty:not([hidden])").waitFor();
  // Confirm Live View notices a genuinely new collector snapshot on its timer.
  await page.goto(base + "/sources");
  await page
    .getByRole("button", { name: "Show token", exact: true })
    .first()
    .click();
  const previewToken = (await page.locator("pre.token").innerText()).trim();
  const raw = JSON.parse(
    await fs.readFile("internal/dashboard/testdata/rich-hems.json", "utf8"),
  );
  raw.telemetry_sequence = Date.now();
  raw.hems.captured_at_utc = Math.floor(Date.now() / 1000);
  await page.goto(
    base +
      "/live?sender=1&range=custom&from=2020-01-01T00:00:00Z&to=2020-01-02T00:00:00Z",
  );
  await page.locator("#recent a").first().waitFor();
  const previousID = Number(
    (await page.locator("#recent a").first().getAttribute("href"))
      .split("/")
      .pop(),
  );
  const original = JSON.stringify(raw, null, 2) + "\n";
  const ingest = await context.request.post(base + "/api/v1/telemetry", {
    headers: {
      Authorization: "Bearer " + previewToken,
      "Content-Type": "application/json",
    },
    data: original,
  });
  assert.equal(ingest.status(), 200);
  await page
    .locator(`#recent a[href="/uploads/${previousID + 1}"]`)
    .waitFor({ timeout: 25000 });
  const downloaded = await context.request.get(
    base + `/uploads/${previousID + 1}/download`,
  );
  assert.equal(await downloaded.text(), original);
  assert.deepEqual(errors, []);
  console.log(
    "Browser smoke passed: navigation, charts, themes, URL filters, history, empty state, mobile overflow. Artifacts: " +
      artifacts,
  );
} finally {
  await browser.close();
}
