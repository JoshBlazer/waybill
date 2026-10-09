import { expect, test } from "@playwright/test";
import { payWithToken } from "./chain";

// The stage 1 thin slice, end to end, through the real stack: a contractor
// creates an invoice; a payer opens the link and pays mock USDC to the
// deposit address on Anvil; the watcher sees it; the tracking page updates
// live, without a reload.
test("an invoice is paid on-chain and the tracking page updates live", async ({
  page,
}) => {
  // Contractor creates an invoice.
  await page.goto("/invoices/new");
  await page.getByLabel("What is it for?").fill("E2E: logo design");
  await page.getByLabel("Amount in USDC").fill("12.34");
  await page.getByRole("button", { name: "Create invoice" }).click();
  await expect(
    page.getByRole("heading", { name: "Invoice created" }),
  ).toBeFocused();
  await expect(page.getByText("12.34 USDC")).toBeVisible();

  // Payer opens the link: the verified name comes before the address.
  await page.getByRole("link", { name: "Open payment link" }).click();
  await expect(
    page.getByRole("heading", { level: 1, name: "Adaeze Okafor" }),
  ).toBeVisible();
  await expect(page.getByRole("img", { name: /Name verified/ })).toBeVisible();
  const address = (
    await page.getByTestId("deposit-address").first().innerText()
  ).trim();
  const token = (
    await page
      .locator("text=Send only the test USDC token")
      .locator("span.font-mono")
      .first()
      .innerText()
  ).trim();
  expect(address).toMatch(/^0x[0-9a-fA-F]{40}$/);

  // Open the tracking page before paying, so the update must arrive live.
  await page.getByRole("link", { name: /track the payment/ }).click();
  await expect(
    page.getByText("Waiting for the payment to arrive."),
  ).toBeVisible();
  await expect(page.getByText("Live")).toBeVisible(); // stream connected

  await payWithToken(token, address, BigInt(12_340_000));

  await expect(page.getByText("We received the payment.")).toBeVisible();
  await expect(
    page.getByText("Waiting for the network to make the payment final.", {
      exact: false,
    }),
  ).toBeVisible();
});

// Settlement needs the hand-written ledger engine (PR #1): confirmation and
// finality post ledger entries. This test fails until the engine exists.
test("the paid invoice becomes final and is shown as paid in full", async ({
  page,
}) => {
  await page.goto("/invoices/new");
  await page.getByLabel("What is it for?").fill("E2E: settlement");
  await page.getByLabel("Amount in USDC").fill("5");
  await page.getByRole("button", { name: "Create invoice" }).click();
  await page.getByRole("link", { name: "Open payment link" }).click();
  const address = (
    await page.getByTestId("deposit-address").first().innerText()
  ).trim();
  const token = (
    await page
      .locator("text=Send only the test USDC token")
      .locator("span.font-mono")
      .first()
      .innerText()
  ).trim();
  await page.getByRole("link", { name: /track the payment/ }).click();

  await payWithToken(token, address, BigInt(5_000_000));

  await expect(
    page.getByText("The payment is final and can't be undone."),
  ).toBeVisible({ timeout: 60_000 });
  await expect(page.getByRole("img", { name: /paid in full/ })).toBeVisible();
});
