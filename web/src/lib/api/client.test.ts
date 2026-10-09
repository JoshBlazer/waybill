import { afterEach, describe, expect, it, vi } from "vitest";
import { fetchHealth, type Health } from "./client";

const body: Health = {
  status: "ok",
  version: "t",
  database: "ok",
  networks: [],
  testMode: true,
};

function mockFetch(impl: () => Promise<Response>) {
  vi.stubGlobal("fetch", vi.fn(impl));
}

afterEach(() => vi.unstubAllGlobals());

describe("fetchHealth", () => {
  it("maps 200 to ok", async () => {
    mockFetch(async () => Response.json(body, { status: 200 }));
    expect(await fetchHealth("http://api")).toEqual({
      kind: "ok",
      health: body,
    });
  });

  it("maps 503 to degraded, keeping the body", async () => {
    const degraded = { ...body, status: "degraded", database: "unavailable" };
    mockFetch(async () => Response.json(degraded, { status: 503 }));
    expect(await fetchHealth("http://api")).toEqual({
      kind: "degraded",
      health: degraded,
    });
  });

  it("maps a network failure to unreachable, not degraded", async () => {
    mockFetch(async () => {
      throw new TypeError("fetch failed");
    });
    expect((await fetchHealth("http://api")).kind).toBe("unreachable");
  });

  it("maps an unexpected status to unreachable", async () => {
    mockFetch(async () => new Response("bad gateway", { status: 502 }));
    expect(await fetchHealth("http://api")).toEqual({
      kind: "unreachable",
      reason: "HTTP 502",
    });
  });
});
