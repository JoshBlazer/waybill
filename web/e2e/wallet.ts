// A stand-in browser wallet (EIP-1193) for end-to-end tests. It answers
// account and chain questions itself and forwards everything else, including
// eth_sendTransaction, to Anvil, whose dev accounts are unlocked. The page's
// real wagmi injected-connector code runs unchanged.
import type { Page } from "@playwright/test";

export async function installTestWallet(
  page: Page,
  rpcUrl: string,
  account: string,
) {
  await page.addInitScript(
    ([rpc, from]) => {
      const listeners: Record<string, Array<(arg: unknown) => void>> = {};
      let id = 0;
      (window as unknown as { ethereum: unknown }).ethereum = {
        isWaybillTestWallet: true,
        async request({
          method,
          params,
        }: {
          method: string;
          params?: unknown[];
        }) {
          switch (method) {
            case "eth_requestAccounts":
            case "eth_accounts":
              return [from];
            case "eth_chainId":
              return "0x7a69"; // 31337
            case "wallet_switchEthereumChain":
            case "wallet_addEthereumChain":
            case "wallet_requestPermissions":
              return null;
            case "wallet_getPermissions":
              return [{ parentCapability: "eth_accounts" }];
          }
          const res = await fetch(rpc, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
              jsonrpc: "2.0",
              id: ++id,
              method,
              params: params ?? [],
            }),
          });
          const body = await res.json();
          if (body.error)
            throw Object.assign(new Error(body.error.message), {
              code: body.error.code,
            });
          return body.result;
        },
        on(event: string, fn: (arg: unknown) => void) {
          (listeners[event] ??= []).push(fn);
        },
        removeListener(event: string, fn: (arg: unknown) => void) {
          listeners[event] = (listeners[event] ?? []).filter((f) => f !== fn);
        },
      };
    },
    [rpcUrl, account] as const,
  );
}
