import { describe, expect, it } from "vitest";
import { formatLagosDate } from "./dates";

describe("formatLagosDate", () => {
  it("shows Lagos time (UTC+1) with no daylight saving", () => {
    expect(formatLagosDate("2026-10-16T14:29:57Z")).toBe(
      "on Fri 16 Oct at 15:29 (Lagos time)",
    );
    expect(formatLagosDate("2026-12-31T23:30:00Z")).toBe(
      "on Fri 1 Jan at 00:30 (Lagos time)",
    );
  });
});
