import { connection } from "next/server";
import { Suspense } from "react";
import { ServiceStatus } from "@/components/ServiceStatus";
import { fetchHealth } from "@/lib/api/client";

// Stage 0 placeholder: shows that the web app reaches the API through the
// generated contract. The product screens arrive in stage 1.

async function LiveStatus() {
  await connection(); // read at request time, never at build time
  const apiUrl = process.env.API_INTERNAL_URL ?? "http://localhost:8080";
  const result = await fetchHealth(apiUrl, {
    signal: AbortSignal.timeout(3000),
  });
  return <ServiceStatus result={result} />;
}

export default function Home() {
  return (
    <main
      id="main"
      className="mx-auto flex w-full max-w-xl flex-1 flex-col gap-8 px-4 py-12"
    >
      <header className="flex flex-col gap-2">
        <p className="font-mono text-sm text-ink-muted">
          WB-0000-0000-0000-0000
        </p>
        <h1 className="text-3xl font-semibold">Waybill</h1>
        <p className="max-w-[68ch] text-lg">
          Pay Nigerian contractors in stablecoins. They receive naira in their
          bank. Every payment is trackable like a parcel.
        </p>
        <p className="text-ink-muted">Test money only. Nothing here is real.</p>
      </header>

      <section
        aria-labelledby="status-heading"
        className="perforated flex flex-col gap-4 px-6 py-8"
      >
        <h2 id="status-heading" className="text-xl font-semibold">
          Service status
        </h2>
        <Suspense
          fallback={<p className="text-ink-muted">Checking the service…</p>}
        >
          <LiveStatus />
        </Suspense>
      </section>
    </main>
  );
}
