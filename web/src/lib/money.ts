// Exact amount formatting. Amounts arrive as { minor: string, scale } and
// are never converted to a JavaScript number, which would round values
// above 2^53 and misrepresent decimals.
import type { components } from "./api/schema";

export type Amount = components["schemas"]["Amount"];

/** Major-unit decimal string, trailing zeros trimmed to at least `minFraction`
 *  places: { minor: "125500000", scale: 6 } → "125.50". Thousands are grouped
 *  with commas. */
export function formatAmount(a: Amount, minFraction = 2): string {
  const negative = a.minor.startsWith("-");
  const digits = (negative ? a.minor.slice(1) : a.minor).padStart(
    a.scale + 1,
    "0",
  );
  const whole = digits.slice(0, digits.length - a.scale);
  let fraction = a.scale > 0 ? digits.slice(digits.length - a.scale) : "";
  const keep = Math.min(minFraction, a.scale);
  while (fraction.length > keep && fraction.endsWith("0")) {
    fraction = fraction.slice(0, -1);
  }
  const grouped = whole.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  return (negative ? "-" : "") + grouped + (fraction ? "." + fraction : "");
}

/** "125.50 USDC" */
export function formatAmountWithAsset(a: Amount): string {
  return `${formatAmount(a)} ${a.asset}`;
}
