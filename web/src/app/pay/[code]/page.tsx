import type { Metadata } from "next";
import Link from "next/link";
import { Suspense } from "react";
import { Stamp } from "@/components/Stamp";
import { apiBaseUrl, fetchPaymentLink } from "@/lib/api/client";
import { formatAmountWithAsset } from "@/lib/money";
import { CopyButton } from "./CopyButton";
import { WalletPayButton } from "./WalletPayButton";

export const metadata: Metadata = {
  title: "Pay an invoice · Waybill",
  robots: { index: false, follow: false },
};

const networkNames: Record<string, string> = {
  "evm:31337": "Local test chain (Anvil)",
  "evm:84532": "Base Sepolia (test)",
  "evm:11155111": "Ethereum Sepolia (test)",
};

const dateFormat = new Intl.DateTimeFormat("en-NG", {
  day: "numeric",
  month: "long",
  year: "numeric",
  timeZone: "Africa/Lagos",
});

// What a payer sees. The contractor's verified name comes before any
// address, so a payer checks who they are paying first (docs/RISKS.md §2).
export default function PayPage({ params }: PageProps<"/pay/[code]">) {
  return (
    <main
      id="main"
      className="mx-auto flex w-full max-w-xl flex-1 flex-col gap-6 px-4 py-8"
    >
      <Suspense
        fallback={
          <div
            aria-busy="true"
            aria-label="Loading payment link"
            className="h-96 bg-rule/40"
          />
        }
      >
        <PayContent params={params} />
      </Suspense>
    </main>
  );
}

async function PayContent({ params }: { params: Promise<{ code: string }> }) {
  const { code } = await params;
  const result = await fetchPaymentLink(apiBaseUrl(), code, {
    signal: AbortSignal.timeout(5000),
  });
  if (result.kind === "not_found") {
    return (
      <section className="flex flex-col gap-3">
        <h1 className="text-2xl font-semibold">
          This payment link doesn&apos;t exist
        </h1>
        <p>
          Check the link you were sent, or ask the contractor for a new one.
        </p>
      </section>
    );
  }
  if (result.kind === "unreachable") {
    return (
      <section className="flex flex-col gap-3">
        <h1 className="text-2xl font-semibold">
          We couldn&apos;t load this payment link
        </h1>
        <p>
          Don&apos;t send any money until this page loads.{" "}
          <a href="" className="underline">
            Try again
          </a>
          .
        </p>
      </section>
    );
  }

  const p = result.value;
  const closed = p.state !== "open" && p.state !== "underpaid";
  return (
    <>
      <header className="flex flex-col gap-3">
        <p className="text-sm text-ink-muted">You are paying</p>
        <div className="flex flex-wrap items-center gap-3">
          <h1 className="text-2xl font-semibold">{p.contractorName}</h1>
          {p.verified ? (
            <Stamp
              variant="confirmed"
              label="Verified"
              description="Name verified against their bank account."
            />
          ) : (
            <Stamp
              variant="problem"
              label="Not verified"
              description="This name has not been verified."
            />
          )}
        </div>
        {p.verified && (
          <p className="text-sm text-ink-muted">
            Waybill checked this name against the contractor&apos;s bank
            account.
          </p>
        )}
      </header>

      <section
        aria-labelledby="amount-heading"
        className="perforated flex flex-col gap-4 px-5 py-7"
      >
        <h2 id="amount-heading" className="sr-only">
          Amount
        </h2>
        <p className="font-mono text-3xl">{formatAmountWithAsset(p.amount)}</p>
        <p>{p.description}</p>
        <p className="text-sm text-ink-muted">
          Link open until {dateFormat.format(new Date(p.expiresAt))}. Tracking
          number <span className="font-mono">{p.trackingCode}</span>.
        </p>
      </section>

      {closed ? (
        <section className="flex flex-col gap-2">
          <p>
            {p.state === "expired"
              ? "This link has expired. Ask the contractor for a new one."
              : "A payment has already been received for this invoice."}
          </p>
          <Link href={`/t/${p.trackingCode}`} className="underline">
            Track the payment
          </Link>
        </section>
      ) : (
        <section aria-labelledby="send-heading" className="flex flex-col gap-4">
          <h2 id="send-heading" className="text-xl font-semibold">
            Pay with stablecoin
          </h2>
          {p.depositAddresses.map((d) => (
            <div
              key={d.network}
              className="flex flex-col gap-2 border border-ink p-4"
            >
              <p className="font-semibold">
                {networkNames[d.network] ?? d.network}
              </p>
              <p className="text-sm text-ink-muted">
                Send exactly {formatAmountWithAsset(p.amount)} to:
              </p>
              <p
                className="font-mono text-sm break-all"
                data-testid="deposit-address"
              >
                {d.address}
              </p>
              <CopyButton value={d.address} label="Copy address" />
              <p className="text-sm">
                Send only the test USDC token{" "}
                <span className="font-mono break-all text-ink-muted">
                  {d.token}
                </span>{" "}
                on this network. Anything else sent here cannot be credited
                automatically.
              </p>
              <WalletPayButton
                network={d.network}
                token={d.token}
                depositAddress={d.address}
                minor={p.amount.minor}
                displayAmount={formatAmountWithAsset(p.amount)}
                trackingCode={p.trackingCode}
              />
            </div>
          ))}
          <Link href={`/t/${p.trackingCode}`} className="underline">
            I&apos;ve sent it: track the payment
          </Link>
        </section>
      )}
      <p className="text-sm text-ink-muted">
        Test money only. Nothing here is real.
      </p>
    </>
  );
}
