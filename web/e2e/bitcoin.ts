// Minimal JSON-RPC helpers for paying on the regtest node in end-to-end
// tests: the "payer" wallet holds mined coins (deploy/bitcoin/setup.sh).

const rpcUrl = () =>
  process.env.E2E_BTC_RPC_URL ??
  "http://waybill:waybill-local-only@localhost:18443";

let id = 0;
async function rpc<T>(
  method: string,
  params: unknown[],
  wallet = "",
): Promise<T> {
  const u = new URL(rpcUrl());
  const auth = Buffer.from(
    `${decodeURIComponent(u.username)}:${decodeURIComponent(u.password)}`,
  ).toString("base64");
  u.username = "";
  u.password = "";
  if (wallet) u.pathname = `/wallet/${wallet}`;
  const res = await fetch(u, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Basic ${auth}`,
    },
    body: JSON.stringify({ jsonrpc: "1.0", id: ++id, method, params }),
  });
  const body = (await res.json()) as {
    result?: T;
    error?: { message: string } | null;
  };
  if (body.error) throw new Error(`${method}: ${body.error.message}`);
  return body.result as T;
}

/** Mines n blocks to the payer wallet. */
export async function mine(n: number): Promise<void> {
  const addr = await rpc<string>("getnewaddress", [], "payer");
  await rpc("generatetoaddress", [n, addr]);
}

/** Pays btc (a decimal string) to address from the payer wallet and mines
 *  one block, returning the txid. */
export async function payBTC(address: string, btc: string): Promise<string> {
  const txid = await rpc<string>(
    "sendtoaddress",
    [address, Number(btc)],
    "payer",
  );
  await mine(1);
  return txid;
}

/** Whether the stack under test watches Bitcoin regtest. */
export async function bitcoinEnabled(): Promise<boolean> {
  const api = process.env.E2E_API_URL ?? "http://localhost:8080";
  try {
    const res = await fetch(new URL("/v1/health", api));
    const h = (await res.json()) as { networks?: string[] };
    return (h.networks ?? []).includes("btc:regtest");
  } catch {
    return false;
  }
}
