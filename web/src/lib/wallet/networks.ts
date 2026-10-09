// Waybill network ids → viem chains. Only test networks exist here, matching
// the API's test-money guard: a wallet cannot be asked to pay on mainnet.
import { anvil, baseSepolia, sepolia } from "viem/chains";
import type { Chain } from "viem";

export const walletChains = [anvil, baseSepolia, sepolia] as const;

const byNetwork: Record<string, Chain> = {
  "evm:31337": anvil,
  "evm:84532": baseSepolia,
  "evm:11155111": sepolia,
};

export function chainForNetwork(network: string): Chain | undefined {
  return byNetwork[network];
}

export type TransferRequest = {
  chainId: number;
  token: `0x${string}`;
  to: `0x${string}`;
  amount: bigint;
};

const hexAddress = /^0x[0-9a-fA-F]{40}$/;

/** Builds the exact ERC-20 transfer for an invoice, or explains why not.
 *  The amount comes from the API's minor-unit string, so it is exact. */
export function transferRequest(input: {
  network: string;
  token: string;
  depositAddress: string;
  minor: string;
}): TransferRequest | { error: string } {
  const chain = chainForNetwork(input.network);
  if (!chain) return { error: `Unsupported network ${input.network}` };
  if (!hexAddress.test(input.token) || !hexAddress.test(input.depositAddress)) {
    return { error: "Invalid address" };
  }
  if (!/^[1-9][0-9]*$/.test(input.minor)) return { error: "Invalid amount" };
  return {
    chainId: chain.id,
    token: input.token as `0x${string}`,
    to: input.depositAddress as `0x${string}`,
    amount: BigInt(input.minor),
  };
}
