import { describe, expect, it } from "vitest";
import { chainForNetwork, transferRequest } from "./networks";

const dep = "0x86BE500b55D4d453EFaB57784Ed6790D4A056423";
const token = "0x5FbDB2315678afecb367f032d93F642f64180aa3";

describe("chainForNetwork", () => {
  it("maps only test networks", () => {
    expect(chainForNetwork("evm:31337")?.id).toBe(31337);
    expect(chainForNetwork("evm:84532")?.id).toBe(84532);
    expect(chainForNetwork("evm:11155111")?.id).toBe(11155111);
    expect(chainForNetwork("evm:1")).toBeUndefined();
    expect(chainForNetwork("evm:8453")).toBeUndefined();
  });
});

describe("transferRequest", () => {
  it("builds an exact transfer", () => {
    expect(
      transferRequest({
        network: "evm:84532",
        token,
        depositAddress: dep,
        minor: "12340000",
      }),
    ).toEqual({ chainId: 84532, token, to: dep, amount: BigInt(12_340_000) });
  });

  it("keeps amounts beyond 2^53 exact", () => {
    const r = transferRequest({
      network: "evm:31337",
      token,
      depositAddress: dep,
      minor: "9007199254740993",
    });
    expect("amount" in r && r.amount.toString()).toBe("9007199254740993");
  });

  it.each([
    [
      { network: "evm:1", token, depositAddress: dep, minor: "1" },
      "Unsupported network",
    ],
    [
      { network: "evm:31337", token: "0x1", depositAddress: dep, minor: "1" },
      "Invalid address",
    ],
    [
      { network: "evm:31337", token, depositAddress: dep, minor: "0" },
      "Invalid amount",
    ],
    [
      { network: "evm:31337", token, depositAddress: dep, minor: "1.5" },
      "Invalid amount",
    ],
    [
      { network: "evm:31337", token, depositAddress: dep, minor: "-5" },
      "Invalid amount",
    ],
  ])("refuses %o", (input, message) => {
    const r = transferRequest(input);
    expect("error" in r && r.error).toContain(message);
  });
});
