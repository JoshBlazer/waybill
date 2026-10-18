import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";

// Stage 3: a contractor adds a bank account (the bank's name for it must
// match their legal name) and creates an invoice paid out in naira at a
// rate locked when it is created. Without a Paystack key the stack uses a
// fake bank that resolves any account to the demo contractor's name.
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

test("a contractor adds a bank account and gets a locked naira rate", async ({
  page,
}) => {
  await page.goto("/account/bank");
  await expect(
    page.getByRole("heading", { level: 1, name: "Bank account" }),
  ).toBeVisible();
  await expectNoViolations(page, "bank account page");

  // A malformed number is refused before anything is saved.
  await page.getByLabel("Bank").selectOption({ label: "Guaranty Trust Bank" });
  await page.getByLabel("Account number").fill("12345");
  await page.getByRole("button", { name: "Check and save" }).click();
  await expect(page.getByLabel("Account number")).toHaveJSProperty(
    "validity.valid",
    false,
  );

  await page.getByLabel("Account number").fill("0123456789");
  await page.getByRole("button", { name: "Check and save" }).click();
  await expect(page.getByRole("status")).toContainText(
    "Saved. The bank holds this account as",
  );
  const current = page.getByRole("region", { name: "Naira goes to" });
  await expect(current).toContainText("Guaranty Trust Bank");
  await expect(current).toContainText("account ending 6789");
  await expectNoViolations(page, "bank account page after saving");

  await page.goto("/invoices/new");
  await expect(page.getByText(/account ending 6789/)).toBeVisible();
  await page.getByLabel("What is it for?").fill("E2E: naira invoice");
  await page.getByLabel("Amount in USDC").fill("100");
  await page.getByLabel(/Naira to my bank/).check();
  await expectNoViolations(page, "new invoice form with naira chosen");
  await page.getByRole("button", { name: "Create invoice" }).click();
  await expect(
    page.getByRole("heading", { name: "Invoice created" }),
  ).toBeFocused();
  // The exact rate depends on the source (CI uses the fixed fake rate); the
  // quote must name an amount, a rate, an expiry and its source.
  await expect(
    page.getByText(
      /You['’]ll receive ₦[\d,]+\.\d{2} at ₦[\d,]+\.\d{2} per USDC, locked until .+\. Rate from .+\./,
    ),
  ).toBeVisible();
  await expectNoViolations(page, "naira invoice created");

  // The payment link offers card or bank transfer only when the API says it
  // can be paid that way (a Paystack test key is configured). Either way
  // the stablecoin addresses are there.
  await page.getByRole("link", { name: "Open payment link" }).click();
  await expect(
    page.getByRole("heading", { name: "Pay with stablecoin" }),
  ).toBeVisible();
  const code = page.url().split("/pay/")[1];
  const api = process.env.E2E_API_URL ?? "http://localhost:8080";
  const link = await (await page.request.get(`${api}/v1/pay/${code}`)).json();
  const naira = page.getByRole("region", { name: "Pay in naira" });
  if (!link.naira) {
    await expect(naira).toHaveCount(0);
    return;
  }
  await expect(naira).toContainText(
    /Pay ₦[\d,]+\.\d{2} by card or bank transfer/,
  );
  await expectNoViolations(page, "payment link with naira");
  await naira
    .getByRole("button", { name: "Pay by card or bank transfer" })
    .click();
  await page.waitForURL(/^https:\/\/checkout\.paystack\.com\//);
});
