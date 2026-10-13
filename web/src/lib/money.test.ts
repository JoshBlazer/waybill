import { describe, expect, it } from "vitest";
import { formatAmount, formatAmountWithAsset } from "./money";

const usdc = (minor: string) => ({ asset: "USDC", minor, scale: 6 });

describe("formatAmount", () => {
  it.each([
    ["125500000", "125.50"],
    ["125000000", "125.00"],
    ["1", "0.000001"],
    ["0", "0.00"],
    ["-2500000", "-2.50"],
    ["1234567890000", "1,234,567.89"],
    ["1234567891234", "1,234,567.891234"],
  ])("USDC %s → %s", (minor, want) => {
    expect(formatAmount(usdc(minor))).toBe(want);
  });

  it("keeps precision beyond 2^53", () => {
    // 9,007,199,254.740993 USDC: Number() would round the last digit.
    expect(formatAmount(usdc("9007199254740993"))).toBe("9,007,199,254.740993");
  });

  it("handles scale 0 and naira kobo", () => {
    expect(formatAmount({ asset: "X", minor: "42", scale: 0 })).toBe("42");
    expect(formatAmount({ asset: "NGN", minor: "154025", scale: 2 })).toBe(
      "1,540.25",
    );
  });

  it("appends the asset", () => {
    expect(formatAmountWithAsset(usdc("25000000"))).toBe("25.00 USDC");
  });
});
