import { readFile } from "node:fs/promises";
import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";
import { mintTo } from "./chain";

// Stage 4: a payer team funds its organisation, builds a run above its
// approval threshold, has a second member approve it, sends it, and sees it
// paid to the contractor's wallet in one transaction.
const DEMO_CONTRACTOR = "00000000-0000-4000-8000-00000000d001";

async function expectNoViolations(page: Page, what: string) {
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
    .analyze();
  const summary = results.violations.map(
    (v) =>
      `${v.id} (${v.impact}): ${v.help}\n` +
      v.nodes.map((n) => `    ${n.target.join(" ")}`).join("\n"),
  );
  expect(summary, `axe violations on ${what}`).toEqual([]);
}

// Next.js keeps pages visited earlier mounted but hidden; only the visible
// one counts.
const visible = (page: Page, id: string) =>
  page.getByTestId(id).filter({ visible: true });

async function actAs(page: Page, name: string) {
  const acting = visible(page, "acting-as");
  if ((await acting.innerText()).trim() !== name) {
    await page.getByRole("button", { name: `Act as ${name}` }).click();
  }
  await expect(acting).toHaveText(name);
}

const usdc = (text: string) => Number(text.replace(/[^0-9.]/g, ""));

test("a run above the threshold needs a second person, then pays in one transaction", async ({
  page,
}) => {
  test.setTimeout(180_000);
  // Stamps land with a 120 ms animation; axe must see their final colours.
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.goto("/org");
  await expect(
    page.getByRole("heading", { level: 1, name: "Lagoon Labs" }),
  ).toBeVisible();
  await actAs(page, "Tolu Bakare");
  await expectNoViolations(page, "organisation page");

  // Fund the organisation with 600 test USDC and wait until it is final.
  const before = usdc(await visible(page, "available").innerText());
  const address = (await visible(page, "funding-address").innerText()).trim();
  const token = (await visible(page, "funding-token").innerText()).trim();
  await mintTo(token, address, BigInt(600_000_000));
  await expect(async () => {
    await page.reload();
    expect(
      usdc(await visible(page, "available").innerText()),
    ).toBeGreaterThanOrEqual(before + 600);
  }).toPass({ timeout: 60_000 });

  // Build a 550 USDC run: above the 500 threshold.
  await page.getByRole("link", { name: "New run" }).click();
  await expectNoViolations(page, "new run form");
  await page.getByLabel("Contractor id").first().fill(DEMO_CONTRACTOR);
  await page.getByLabel("Amount in USDC").first().fill("550");
  await page.getByLabel("Reference").first().fill(`E2E ${Date.now()}`);
  await page.getByRole("button", { name: "Build and check the run" }).click();
  await expect(visible(page, "run-state")).toHaveText("awaiting_approval");
  await expect(
    page.getByText("Someone other than its creator must approve it."),
  ).toBeVisible();
  // The creator is offered no way to approve or send it.
  await expect(
    page.getByRole("button", { name: "Approve the run" }),
  ).toHaveCount(0);
  await expect(page.getByRole("button", { name: /^Send/ })).toHaveCount(0);
  await expectNoViolations(page, "run awaiting approval");

  // The approver approves; the maker sends.
  await actAs(page, "Chidi Eze");
  await page.getByRole("button", { name: "Approve the run" }).click();
  await expect(visible(page, "run-state")).toHaveText("approved");
  await actAs(page, "Tolu Bakare");
  await page.getByRole("button", { name: "Send 550.00 USDC" }).click();
  await expect(visible(page, "run-state")).toHaveText(
    /sending|confirming|paid/,
  );

  await expect(async () => {
    await page.reload();
    await expect(visible(page, "run-state")).toHaveText("paid");
  }).toPass({ timeout: 120_000 });
  await expect(page.getByText(/^Transaction 0x[0-9a-f]{64}$/)).toBeVisible();
  await expectNoViolations(page, "paid run");

  // The run's receipt downloads as a PDF.
  const [receipt] = await Promise.all([
    page.waitForEvent("download"),
    page.getByRole("link", { name: "Download the receipt (PDF)" }).click(),
  ]);
  expect(receipt.suggestedFilename()).toMatch(/^run-[0-9a-f-]{36}\.pdf$/);
  const pdf = await readFile(await receipt.path());
  expect(pdf.subarray(0, 8).toString("latin1")).toBe("%PDF-1.4");
  expect(pdf.toString("latin1")).toContain("(Payment run receipt) Tj");

  // So does the organisation's export, with the run as money out.
  const runUrl = page.url();
  await page.goto("/org");
  const [csv] = await Promise.all([
    page.waitForEvent("download"),
    page.getByRole("button", { name: "Download CSV" }).click(),
  ]);
  const rows = (await readFile(await csv.path()))
    .toString("utf8")
    .trim()
    .split("\n");
  expect(rows[0]).toBe(
    "date,waybill_reference,description,asset,money_in,money_out,balance,chain_tx,provider_reference",
  );
  expect(rows.some((r) => r.includes(",Payment run,USDC,,550.000000,"))).toBe(
    true,
  );
  await page.goto(runUrl);

  // The contractor's line has its own tracking page, delivered.
  await page.getByRole("link", { name: "Track this payment" }).click();
  await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
  await expectNoViolations(page, "tracking page of a batch line");
});
