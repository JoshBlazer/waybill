import type { Metadata } from "next";
import Link from "next/link";
import { connection } from "next/server";
import { Suspense } from "react";
import { apiBaseUrl } from "@/lib/api/client";
import { getFunding, getOrg, listBatches, listMembers } from "@/lib/api/org";
import { currentMember } from "@/lib/member";
import { formatAmountWithAsset } from "@/lib/money";
import { CopyButton } from "../pay/[code]/CopyButton";
import { MemberSwitcher } from "./MemberSwitcher";
import { stateLabel } from "./states";

export const metadata: Metadata = { title: "Organisation · Waybill" };

export default function OrgPage() {
  return (
    <main
      id="main"
      className="mx-auto flex w-full max-w-2xl flex-1 flex-col gap-6 px-4 py-8"
    >
      <Suspense
        fallback={
          <div
            role="status"
            aria-busy="true"
            aria-label="Loading the organisation"
            className="h-96 bg-rule/40"
          />
        }
      >
        <Loaded />
      </Suspense>
      <p className="text-sm text-ink-muted">
        Test money only. Nothing here is real.
      </p>
    </main>
  );
}

async function Loaded() {
  await connection();
  const { who, key } = await currentMember();
  if (!key) {
    return (
      <p role="alert">The demo organisation is not set up on this server.</p>
    );
  }
  const base = apiBaseUrl();
  const [org, members, funding, runs] = await Promise.all([
    getOrg(base, key),
    listMembers(base, key),
    getFunding(base, key),
    listBatches(base, key),
  ]);
  if (
    org.kind !== "ok" ||
    members.kind !== "ok" ||
    funding.kind !== "ok" ||
    runs.kind !== "ok"
  ) {
    return (
      <p role="alert" className="border-l-[3px] border-ink pl-3">
        We can&apos;t load the organisation right now. Try again in a minute.
      </p>
    );
  }
  const o = org.value;
  const f = funding.value;
  return (
    <>
      <header className="flex flex-col gap-2">
        <p className="text-sm text-ink-muted">Payer organisation</p>
        <h1 className="text-2xl font-semibold">{o.name}</h1>
        <p>
          Runs above{" "}
          <span className="font-mono">
            {formatAmountWithAsset(o.approvalThreshold)}
          </span>{" "}
          need a second person to approve them.
        </p>
      </header>
      <MemberSwitcher current={who} back="/org" />

      <section
        aria-labelledby="funding-heading"
        className="flex flex-col gap-3"
      >
        <h2 id="funding-heading" className="text-xl font-semibold">
          Funds
        </h2>
        <dl className="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1">
          <dt>Available for runs</dt>
          <dd className="font-mono" data-testid="available">
            {formatAmountWithAsset(f.available)}
          </dd>
          <dt>Arriving</dt>
          <dd className="font-mono">{formatAmountWithAsset(f.pending)}</dd>
        </dl>
        {f.depositAddresses.map((d) => (
          <div
            key={d.network}
            className="flex flex-col gap-2 border border-ink p-4"
          >
            <p className="text-sm text-ink-muted">
              To add funds, send test USDC on {d.network} to:
            </p>
            <p
              className="font-mono text-sm break-all"
              data-testid="funding-address"
            >
              {d.address}
            </p>
            <CopyButton value={d.address} label="Copy address" />
            <p className="text-sm">
              Send only the test USDC token{" "}
              <span
                className="font-mono break-all text-ink-muted"
                data-testid="funding-token"
              >
                {d.token}
              </span>
              . Funds can be used once the payment is final.
            </p>
          </div>
        ))}
      </section>

      <section aria-labelledby="runs-heading" className="flex flex-col gap-3">
        <div className="flex flex-wrap items-baseline justify-between gap-3">
          <h2 id="runs-heading" className="text-xl font-semibold">
            Runs
          </h2>
          <Link href="/org/batches/new" className="font-semibold underline">
            New run
          </Link>
        </div>
        {runs.value.data.length === 0 ? (
          <p>No runs yet.</p>
        ) : (
          <ul className="flex flex-col divide-y divide-rule border-y border-rule">
            {runs.value.data.map((r) => (
              <li
                key={r.id}
                className="flex flex-wrap justify-between gap-2 py-2"
              >
                <Link href={`/org/batches/${r.id}`} className="underline">
                  <span className="font-mono">
                    {formatAmountWithAsset(r.total)}
                  </span>
                </Link>
                <span>{stateLabel[r.state]}</span>
              </li>
            ))}
          </ul>
        )}
      </section>

      <section
        aria-labelledby="members-heading"
        className="flex flex-col gap-3"
      >
        <h2 id="members-heading" className="text-xl font-semibold">
          Members
        </h2>
        <ul className="flex flex-col gap-1">
          {members.value.data.map((m) => (
            <li key={m.id}>
              {m.name}{" "}
              <span className="text-ink-muted">({m.roles.join(", ")})</span>
              {m.removedAt ? " — removed" : ""}
            </li>
          ))}
        </ul>
      </section>
    </>
  );
}
