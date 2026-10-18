import type { Metadata } from "next";
import Link from "next/link";
import { connection } from "next/server";
import { Suspense } from "react";
import { apiBaseUrl } from "@/lib/api/client";
import { getBalance, getConversion, type Conversion } from "@/lib/api/org";
import { formatAmountWithAsset } from "@/lib/money";
import { formatLagosDate } from "../bank/dates";
import { AcceptForm, ConvertForm } from "./ConvertForms";

export const metadata: Metadata = { title: "Balance · Waybill" };

type Search = Promise<{ quote?: string; accepted?: string }>;

export default function BalancePage({
  searchParams,
}: {
  searchParams: Search;
}) {
  return (
    <main
      id="main"
      className="mx-auto flex w-full max-w-xl flex-1 flex-col gap-6 px-4 py-8"
    >
      <header className="flex flex-col gap-1">
        <p className="text-sm text-ink-muted">Demo contractor: Adaeze Okafor</p>
        <h1 className="text-2xl font-semibold">Balance</h1>
        <p>Convert stablecoin you hold to naira, sent to your bank.</p>
      </header>
      <Suspense
        fallback={
          <div
            role="status"
            aria-busy="true"
            aria-label="Loading your balance"
            className="h-64 bg-rule/40"
          />
        }
      >
        <Loaded searchParams={searchParams} />
      </Suspense>
      <p>
        <Link href="/account/bank" className="underline">
          Bank account and export
        </Link>
      </p>
      <p className="text-sm text-ink-muted">
        Test money only. Nothing here is real.
      </p>
    </main>
  );
}

const stateText: Record<string, string> = {
  quoted: "Rate offered",
  expired: "Rate expired",
  accepted: "Converted",
};

const payoutText: Record<string, string> = {
  converted: "waiting to be sent",
  submitting: "sending",
  submitted: "sending",
  unknown: "checking with the bank",
  succeeded: "sent to your bank",
  failed: "failed; the naira is still yours",
  reversed: "returned by the bank",
};

function describe(c: Conversion): string {
  return `${formatAmountWithAsset(c.amount)} → ${formatAmountWithAsset(c.naira)}`;
}

async function Loaded({ searchParams }: { searchParams: Search }) {
  await connection();
  const key = process.env.WAYBILL_DEV_CONTRACTOR_KEY;
  if (!key) {
    return <p role="alert">Balances are not set up on this server.</p>;
  }
  const { quote: quoteId, accepted } = await searchParams;
  const base = apiBaseUrl();
  const [bal, quote] = await Promise.all([
    getBalance(base, key),
    quoteId ? getConversion(base, key, quoteId) : Promise.resolve(null),
  ]);
  if (bal.kind !== "ok") {
    return (
      <p role="alert" className="border-l-[3px] border-ink pl-3">
        We can&apos;t load your balance right now. Try again in a minute.
      </p>
    );
  }
  const b = bal.value;
  const q =
    quote?.kind === "ok" && quote.value.state === "quoted" ? quote.value : null;
  return (
    <>
      <section
        aria-labelledby="balance-heading"
        className="flex flex-col gap-3"
      >
        <h2 id="balance-heading" className="text-xl font-semibold">
          What you hold
        </h2>
        <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1">
          <dt>Available</dt>
          <dd className="font-mono" data-testid="usdc-available">
            {formatAmountWithAsset(b.usdcAvailable)}
          </dd>
          <dt>Arriving</dt>
          <dd className="font-mono">{formatAmountWithAsset(b.usdcPending)}</dd>
          <dt>Naira on its way to your bank</dt>
          <dd className="font-mono">
            {formatAmountWithAsset(b.nairaOnItsWay)}
          </dd>
        </dl>
      </section>

      {accepted && (
        <p role="status" className="border-l-[3px] border-ink pl-3">
          Converted. The naira is on its way to your bank.
        </p>
      )}

      <section
        aria-labelledby="convert-heading"
        className="flex flex-col gap-3"
      >
        <h2 id="convert-heading" className="text-xl font-semibold">
          Convert to naira
        </h2>
        {q ? (
          <div className="flex flex-col gap-3 border border-ink p-4">
            <p>
              <span className="font-mono">
                {formatAmountWithAsset(q.amount)}
              </span>{" "}
              becomes{" "}
              <span
                className="font-mono font-semibold"
                data-testid="quote-naira"
              >
                {formatAmountWithAsset(q.naira)}
              </span>{" "}
              at {formatAmountWithAsset(q.rate)} per USDC.
            </p>
            <AcceptForm
              id={q.id}
              expiresAt={q.expiresAt}
              label={`Convert and send ${formatAmountWithAsset(q.naira)}`}
            />
            <p className="text-sm">
              <Link href="/account/balance" className="underline">
                Not now
              </Link>
            </p>
          </div>
        ) : (
          <ConvertForm />
        )}
      </section>

      <section
        aria-labelledby="history-heading"
        className="flex flex-col gap-3"
      >
        <h2 id="history-heading" className="text-xl font-semibold">
          Conversions
        </h2>
        {b.conversions.length === 0 ? (
          <p>No conversions yet.</p>
        ) : (
          <ul className="flex flex-col divide-y divide-rule border-y border-rule">
            {b.conversions.map((c) => (
              <li key={c.id} className="flex flex-col gap-1 py-2">
                <span className="font-mono">{describe(c)}</span>
                <span className="text-sm">
                  {stateText[c.state]}
                  {c.payout
                    ? `, ${payoutText[c.payout.state] ?? c.payout.state}`
                    : ""}{" "}
                  · {formatLagosDate(c.acceptedAt ?? c.createdAt)}
                </span>
              </li>
            ))}
          </ul>
        )}
      </section>
    </>
  );
}
