import type { Metadata } from "next";
import Link from "next/link";
import { connection } from "next/server";
import { Suspense } from "react";
import { apiBaseUrl } from "@/lib/api/client";
import { listPayoutWallets } from "@/lib/api/org";
import { formatLagosDate } from "../bank/dates";
import { WalletForm } from "./WalletForm";

export const metadata: Metadata = { title: "Payout wallet · Waybill" };

export default function WalletPage() {
  return (
    <main
      id="main"
      className="mx-auto flex w-full max-w-xl flex-1 flex-col gap-6 px-4 py-8"
    >
      <header className="flex flex-col gap-1">
        <p className="text-sm text-ink-muted">Demo contractor: Adaeze Okafor</p>
        <h1 className="text-2xl font-semibold">Payout wallet</h1>
        <p>Where teams that pay you in a batch run send your stablecoin.</p>
      </header>
      <Suspense
        fallback={
          <div
            role="status"
            aria-busy="true"
            aria-label="Loading your wallet"
            className="h-48 bg-rule/40"
          />
        }
      >
        <Loaded />
      </Suspense>
      <p>
        <Link href="/account/bank" className="underline">
          Bank account
        </Link>
      </p>
      <p className="text-sm text-ink-muted">
        Test money only. Nothing here is real.
      </p>
    </main>
  );
}

async function Loaded() {
  await connection();
  const key = process.env.WAYBILL_DEV_CONTRACTOR_KEY;
  if (!key) {
    return <p role="alert">Wallets are not set up on this server.</p>;
  }
  const r = await listPayoutWallets(apiBaseUrl(), key);
  if (r.kind !== "ok") {
    return (
      <p role="alert" className="border-l-[3px] border-ink pl-3">
        We can&apos;t load your wallet right now. Try again in a minute.
      </p>
    );
  }
  const now = new Date();
  return (
    <>
      {r.value.data.map((w) => (
        <section
          key={w.network}
          aria-label={`Wallet on ${w.network}`}
          className="flex flex-col gap-1 border border-ink p-4"
        >
          <p className="text-sm text-ink-muted">{w.network}</p>
          <p className="font-mono text-sm break-all">{w.address}</p>
          <p className="text-sm">
            {new Date(w.payoutsAllowedFrom) > now
              ? `New wallet: runs can pay it ${formatLagosDate(w.payoutsAllowedFrom)}.`
              : "Runs can pay this wallet."}
          </p>
        </section>
      ))}
      <WalletForm network={r.value.data[0]?.network ?? "evm:31337"} />
    </>
  );
}
