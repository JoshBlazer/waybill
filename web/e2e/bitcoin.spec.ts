import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";
import { bitcoinEnabled, mine, payBTC } from "./bitcoin";

// Stage 5 exit check, Bitcoin half: the thin-slice path on Bitcoin regtest.
// A contractor creates a BTC invoice, the payer pays the derived address,
// and the tracking page shows it received and then final. Runs when the
// stack watches btc:regtest (make up-bitcoin); skipped otherwise.
async function expectNoViolations(page: Page, what: string) {
  await expect(page).toHaveTitle(/Waybill/);
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

test("a BTC invoice is paid on regtest and becomes final", async ({ page }) => {
  test.skip(!(await bitcoinEnabled()), "the stack does not watch btc:regtest");
  test.setTimeout(180_000);

  await page.goto("/invoices/new");
  await page.getByLabel("Bitcoin on a test network (held in BTC)").check();
  await page.getByLabel("What is it for?").fill("E2E: bitcoin");
  await page.getByLabel("Amount in BTC").fill("0.015");
  // Naira is for USDC invoices only.
  await expect(page.getByLabel(/Naira to my bank/)).toBeDisabled();
  await expectNoViolations(page, "new invoice form with BTC chosen");
  await page.getByRole("button", { name: "Create invoice" }).click();
  await expect(
    page.getByRole("heading", { name: "Invoice created" }),
  ).toBeFocused();
  await expect(page.getByText("0.015 BTC")).toBeVisible();

  await page.getByRole("link", { name: "Open payment link" }).click();
  await expect(
    page.getByRole("heading", { name: "Pay with bitcoin" }),
  ).toBeVisible();
  const address = (
    await page.getByTestId("deposit-address").first().innerText()
  ).trim();
  expect(address).toMatch(/^bcrt1q[02-9ac-hj-np-z]{38,}$/);
  await expect(
    page.getByRole("link", { name: "Open in a Bitcoin wallet" }),
  ).toHaveAttribute("href", `bitcoin:${address}?amount=0.015`);
  await expectNoViolations(page, "bitcoin payment page");

  await page.getByRole("link", { name: /track the payment/ }).click();
  await expect(page.getByText("Live")).toBeVisible();
  await payBTC(address, "0.015");
  await expect(page.getByText("We received the payment.")).toBeVisible({
    timeout: 30_000,
  });
  await mine(3); // final at 3 blocks on regtest
  await expect(
    page.getByText("The payment is final and can't be undone."),
  ).toBeVisible({ timeout: 60_000 });
  await expect(page.getByRole("img", { name: /paid in full/ })).toBeVisible();
});
