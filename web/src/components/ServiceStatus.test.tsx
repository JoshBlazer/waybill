import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { Health } from "@/lib/api/client";
import { ServiceStatus } from "./ServiceStatus";

const health: Health = {
  status: "ok",
  version: "0.1.0-test",
  database: "ok",
  networks: ["evm:31337"],
  testMode: true,
};

describe("ServiceStatus", () => {
  it("shows a running stamp and plain-language details when healthy", () => {
    render(<ServiceStatus result={{ kind: "ok", health }} />);
    expect(
      screen.getByRole("img", { name: "The Waybill service is running." }),
    ).toBeDefined();
    expect(screen.getByText("Answering")).toBeDefined();
    expect(screen.getByText("evm:31337")).toBeDefined();
    expect(screen.getByText("Test money only")).toBeDefined();
  });

  it("reports a degraded database as a problem", () => {
    render(
      <ServiceStatus
        result={{
          kind: "degraded",
          health: { ...health, status: "degraded", database: "unavailable" },
        }}
      />,
    );
    expect(
      screen.getByRole("img", { name: /database is unavailable/ }),
    ).toBeDefined();
    expect(screen.getByText("Not answering")).toBeDefined();
  });

  it("never presents 'unreachable' as 'down'", () => {
    render(
      <ServiceStatus
        result={{ kind: "unreachable", reason: "network error" }}
      />,
    );
    expect(
      screen.getByRole("img", {
        name: "We couldn't reach the Waybill service.",
      }),
    ).toBeDefined();
    expect(screen.getByText(/not the same as it being down/)).toBeDefined();
  });
});
