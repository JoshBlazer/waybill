import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";
import { mintTo } from "./chain";

// Stage 4: a contractor holding USDC from a paid invoice converts part of it
// to naira at a quoted rate; the naira is queued for their bank.
async function expectNoViolations(page: Page, what: string) {
  // Next.js streams metadata: after a redirect the title can arrive a
  // moment after the body, and axe must see the finished page.
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

const usdc = (text: string) => Number(text.replace(/[^0-9.]/g, ""));

test("a contractor converts part of their USDC balance to naira", async ({
  page,
}) => {
  test.setTimeout(180_000);
  // A verified bank account (the fake bank resolves it without a key).
  await page.goto("/account/bank");
  // Exact: the loading placeholder is labelled "Loading your bank account".
  const bank = page.getByLabel("Bank", { exact: true });
  await expect(bank).toBeVisible();
  await bank.selectOption({ label: "Guaranty Trust Bank" });
  await page.getByLabel("Account number").fill("0123456789");
  await page.getByRole("button", { name: "Check and save" }).click();
  await expect(page.getByRole("status")).toContainText("Saved.");

  // A held USDC invoice, paid and final, adds to the balance.
  await page.goto("/invoices/new");
  await page.getByLabel("What is it for?").fill("E2E: conversion");
  await page.getByLabel("Amount in USDC").fill("3");
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
  await mintTo(token, address, BigInt(3_000_000));
  await expect(
    page.getByText("The payment is final and can't be undone."),
  ).toBeVisible({ timeout: 60_000 });

  await page.goto("/account/balance");
  await expect(
    page.getByRole("heading", { level: 1, name: "Balance" }),
  ).toBeVisible();
  const before = usdc(await page.getByTestId("usdc-available").innerText());
  expect(before).toBeGreaterThanOrEqual(3);
  await expectNoViolations(page, "balance page");

  await page.getByLabel("USDC to convert").fill("2");
  await page.getByRole("button", { name: "Get a rate" }).click();
  await expect(page.getByTestId("quote-naira")).toContainText("NGN");
  await expect(page.getByTestId("quote-countdown")).toHaveText(/^[0-2]:\d\d$/);
  await expectNoViolations(page, "balance page with a quote");

  await page.getByRole("button", { name: /^Convert and send/ }).click();
  await expect(page.getByRole("status")).toContainText(
    "Converted. The naira is on its way to your bank.",
  );
  await expect(page.getByTestId("usdc-available")).toHaveText(
    `${(before - 2).toFixed(2)} USDC`,
  );
  await expect(
    page
      .getByRole("region", { name: "Conversions" })
      .getByText(/^Converted/)
      .first(),
  ).toBeVisible();
  await expectNoViolations(page, "balance page after converting");
});
