"use client";

// Pay an invoice from a browser wallet. Loaded only when the payer asks for
// it (see WalletPayButton), so wagmi and viem never weigh on first load or
// on the tracking page (docs/DESIGN.md §6).
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import Link from "next/link";
import { useState } from "react";
import { BaseError, erc20Abi, UserRejectedRequestError } from "viem";
import {
  createConfig,
  http,
  useConnect,
  useConnection,
  useSwitchChain,
  useWriteContract,
  WagmiProvider,
} from "wagmi";
import { injected } from "wagmi/connectors";
import {
  chainForNetwork,
  transferRequest,
  walletChains,
} from "@/lib/wallet/networks";

const config = createConfig({
  chains: walletChains,
  connectors: [injected()],
  transports: {
    [walletChains[0].id]: http(
      process.env.NEXT_PUBLIC_ANVIL_RPC_URL ?? "http://127.0.0.1:58545",
    ),
    [walletChains[1].id]: http(),
    [walletChains[2].id]: http(),
  },
  ssr: true,
});
const queryClient = new QueryClient();

export type WalletPayProps = {
  network: string;
  token: string;
  depositAddress: string;
  minor: string;
  displayAmount: string; // "12.34 USDC"
  trackingCode: string;
};

export default function WalletPay(props: WalletPayProps) {
  return (
    <WagmiProvider config={config}>
      <QueryClientProvider client={queryClient}>
        <Flow {...props} />
      </QueryClientProvider>
    </WagmiProvider>
  );
}

function rejectedByUser(err: unknown): boolean {
  return (
    err instanceof BaseError &&
    Boolean(err.walk((e) => e instanceof UserRejectedRequestError))
  );
}

function Flow(props: WalletPayProps) {
  const connection = useConnection();
  const connect = useConnect();
  const switchChain = useSwitchChain();
  const write = useWriteContract();
  const [message, setMessage] = useState<string>("");
  const [sentHash, setSentHash] = useState<string>("");

  const chain = chainForNetwork(props.network);
  const request = transferRequest({
    network: props.network,
    token: props.token,
    depositAddress: props.depositAddress,
    minor: props.minor,
  });
  if (!chain || "error" in request) {
    return (
      <p>
        This payment can&apos;t be sent from a browser wallet. Use the address
        above.
      </p>
    );
  }

  const hasInjected = typeof window !== "undefined" && "ethereum" in window;
  if (!hasInjected) {
    return (
      <p>
        No browser wallet found. Send the exact amount to the address above
        instead.
      </p>
    );
  }

  if (sentHash) {
    return (
      <div className="flex flex-col gap-2" role="status">
        <p className="font-semibold">Sent from your wallet.</p>
        <p className="text-sm">
          Transaction <span className="font-mono break-all">{sentHash}</span>
        </p>
        <Link href={`/t/${props.trackingCode}`} className="underline">
          Track the payment
        </Link>
      </div>
    );
  }

  const button =
    "min-h-11 self-start border-2 border-ink bg-ink px-5 font-semibold text-paper disabled:opacity-60";
  const busy = connect.isPending || switchChain.isPending || write.isPending;

  async function run(step: () => Promise<unknown>) {
    setMessage("");
    try {
      await step();
    } catch (err) {
      setMessage(
        rejectedByUser(err)
          ? "You cancelled in your wallet. Nothing was sent."
          : "Your wallet reported a problem. Something may or may not have been sent: check your wallet's activity before trying again.",
      );
    }
  }

  let action;
  if (!connection.isConnected) {
    action = (
      <button
        type="button"
        className={button}
        disabled={busy}
        onClick={() =>
          run(() => connect.mutateAsync({ connector: injected() }))
        }
      >
        {connect.isPending ? "Waiting for your wallet…" : "Connect wallet"}
      </button>
    );
  } else if (connection.chainId !== chain.id) {
    action = (
      <button
        type="button"
        className={button}
        disabled={busy}
        onClick={() =>
          run(() => switchChain.mutateAsync({ chainId: chain.id }))
        }
      >
        {switchChain.isPending
          ? "Waiting for your wallet…"
          : `Switch to ${chain.name}`}
      </button>
    );
  } else {
    action = (
      <button
        type="button"
        className={button}
        disabled={busy}
        onClick={() =>
          run(async () => {
            const hash = await write.mutateAsync({
              chainId: request.chainId,
              address: request.token,
              abi: erc20Abi,
              functionName: "transfer",
              args: [request.to, request.amount],
            });
            setSentHash(hash); // the button is gone from here on: no double send
          })
        }
      >
        {write.isPending
          ? "Confirm in your wallet…"
          : `Send ${props.displayAmount}`}
      </button>
    );
  }

  return (
    <div className="flex flex-col gap-3">
      {connection.address && (
        <p className="text-sm text-ink-muted">
          Connected:{" "}
          <span className="font-mono break-all">{connection.address}</span>
        </p>
      )}
      {action}
      {message && (
        <p role="alert" className="border-l-[3px] border-ink pl-3">
          ! {message}
        </p>
      )}
    </div>
  );
}
