// Minimal JSON-RPC helpers for paying on Anvil in end-to-end tests. Anvil's
// dev accounts are unlocked, so eth_sendTransaction needs no private key.

const rpcUrl = () => process.env.E2E_RPC_URL ?? "http://localhost:58545";

/** Anvil dev account #1, used as the payer. */
export const payer = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8";

let id = 0;
async function rpc<T>(method: string, params: unknown[]): Promise<T> {
  const res = await fetch(rpcUrl(), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ jsonrpc: "2.0", id: ++id, method, params }),
  });
  const body = (await res.json()) as {
    result?: T;
    error?: { message: string };
  };
  if (body.error) throw new Error(`${method}: ${body.error.message}`);
  return body.result as T;
}

const word = (hex: string) =>
  hex.replace(/^0x/, "").toLowerCase().padStart(64, "0");
const uint = (n: bigint) => n.toString(16).padStart(64, "0");

// keccak256("mint(address,uint256)")[:4] and keccak256("transfer(address,uint256)")[:4]
const MINT = "0x40c10f19";
const TRANSFER = "0xa9059cbb";

async function send(to: string, data: string): Promise<void> {
  const hash = await rpc<string>("eth_sendTransaction", [
    { from: payer, to, data },
  ]);
  for (let i = 0; i < 60; i++) {
    const receipt = await rpc<{ status: string } | null>(
      "eth_getTransactionReceipt",
      [hash],
    );
    if (receipt) {
      if (receipt.status !== "0x1")
        throw new Error(`transaction ${hash} reverted`);
      return;
    }
    await new Promise((r) => setTimeout(r, 500));
  }
  throw new Error(`transaction ${hash} not mined`);
}

/** Mint `amount` minor units of the mock token to the payer, then send them
 *  to `depositAddress`, exactly as a payer's wallet would. */
export async function payWithToken(
  token: string,
  depositAddress: string,
  amount: bigint,
) {
  await send(token, MINT + word(payer) + uint(amount));
  await send(token, TRANSFER + word(depositAddress) + uint(amount));
}

/** Anvil dev account #2, used as a browser-wallet payer. */
export const walletPayer = "0x3C44CdDdB6a900fa2b585dd299e03d12FA4293BC";

/** Mint test tokens to an account (minting is open on the mock token). */
export async function mintTo(token: string, account: string, amount: bigint) {
  await send(token, MINT + word(account) + uint(amount));
}

export function browserRpcUrl(): string {
  return rpcUrl();
}
