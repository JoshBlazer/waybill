import AxeBuilder from "@axe-core/playwright";
import { expect, test, type Page } from "@playwright/test";

// WCAG 2.1 A and AA checks with axe on every page of the thin slice,
// including the states a user reaches by interacting (docs/DESIGN.md §5).
// Any violation fails the test; the report names the rule and the nodes.
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

test("every thin-slice page passes axe (WCAG 2.1 AA)", async ({ page }) => {
  await page.goto("/");
  await expect(
    page.getByRole("img", { name: /The Waybill service is running\./ }),
  ).toBeVisible();
  await expectNoViolations(page, "home");

  await page.goto("/invoices/new");
  await expectNoViolations(page, "new invoice form");

  // The form's error state: a zero amount passes the browser's checks and
  // is refused by the API.
  await page.getByLabel("What is it for?").fill("E2E: accessibility");
  await page.getByLabel("Amount in USDC").fill("0");
  await page.getByRole("button", { name: "Create invoice" }).click();
  await expect(page.getByRole("alert").first()).toBeVisible();
  await expectNoViolations(page, "new invoice form with errors");

  await page.getByLabel("Amount in USDC").fill("3");
  await page.getByRole("button", { name: "Create invoice" }).click();
  await expect(
    page.getByRole("heading", { name: "Invoice created" }),
  ).toBeVisible();
  await expectNoViolations(page, "invoice created");

  await page.getByRole("link", { name: "Open payment link" }).click();
  await expect(page.getByTestId("deposit-address").first()).toBeVisible();
  await expectNoViolations(page, "payment link");

  await page.getByRole("button", { name: "Pay with a browser wallet" }).click();
  // No wallet is installed in this browser: the panel says so.
  await expect(page.getByText("No browser wallet found.")).toBeVisible();
  await expectNoViolations(page, "payment link with wallet panel open");

  await page.getByRole("link", { name: /track the payment/ }).click();
  await expect(page.getByText("Live")).toBeVisible();
  await expectNoViolations(page, "tracking page");

  await page.goto("/t/WB-0000-0000-0000-0000");
  await expect(
    page.getByRole("heading", { name: "We can't find this payment" }),
  ).toBeVisible();
  await expectNoViolations(page, "unknown tracking number");
});
