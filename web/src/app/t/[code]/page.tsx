import type { Metadata } from "next";
import { Suspense } from "react";
import { apiBaseUrl, fetchTracking } from "@/lib/api/client";
import { formatAmountWithAsset } from "@/lib/money";
import { LiveTracking } from "./LiveTracking";

export const metadata: Metadata = {
  title: "Track a payment · Waybill",
  // Tracking pages are private by link: keep them out of search engines.
  robots: { index: false, follow: false },
};

// The tracking page: server-rendered and readable with JavaScript off; the
// live island only adds updates. docs/DESIGN.md §4.1.
export default function TrackingPage({ params }: PageProps<"/t/[code]">) {
  return (
    <main
      id="main"
      className="mx-auto flex w-full max-w-xl flex-1 flex-col gap-6 px-4 py-8"
    >
      <Suspense fallback={<TrackingSkeleton />}>
        <TrackingContent params={params} />
      </Suspense>
    </main>
  );
}

async function TrackingContent({
  params,
}: {
  params: Promise<{ code: string }>;
}) {
  const { code } = await params;
  const result = await fetchTracking(apiBaseUrl(), code, {
    signal: AbortSignal.timeout(5000),
  });

  if (result.kind === "not_found") {
    return (
      <section className="flex flex-col gap-3">
        <h1 className="text-2xl font-semibold">
          We can&apos;t find this payment
        </h1>
        <p>
          We can&apos;t find a payment with this tracking number. Check the link
          you were sent.
        </p>
      </section>
    );
  }
  if (result.kind === "unreachable") {
    return (
      <section className="flex flex-col gap-3">
        <h1 className="text-2xl font-semibold">
          We couldn&apos;t load this page
        </h1>
        <p>
          Waybill didn&apos;t answer in time. Your payment is not affected.{" "}
          <a href="" className="underline">
            Try again
          </a>
          .
        </p>
      </section>
    );
  }

  const t = result.value;
  return (
    <>
      <header className="flex flex-col gap-1">
        <p className="text-sm text-ink-muted">Tracking number</p>
        <h1 className="font-mono text-2xl font-medium tracking-wide break-all">
          {t.trackingCode}
        </h1>
        <p className="text-lg">
          <span className="font-mono">{formatAmountWithAsset(t.amount)}</span>{" "}
          to {t.contractorName}
        </p>
      </header>
      <section
        aria-label="Status"
        className="perforated flex flex-col gap-6 px-5 py-7"
      >
        <LiveTracking code={t.trackingCode} initial={t} />
      </section>
      <p className="text-sm text-ink-muted">
        Test money only. Nothing here is real.
      </p>
    </>
  );
}

function TrackingSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading payment"
      className="flex flex-col gap-6"
    >
      <div className="h-16 bg-rule/40" />
      <div className="h-72 bg-rule/40" />
    </div>
  );
}
