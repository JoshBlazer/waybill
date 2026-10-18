import type { Metadata } from "next";
import Link from "next/link";
import { connection } from "next/server";
import { Suspense } from "react";
import { Stamp, type StampVariant } from "@/components/Stamp";
import { apiBaseUrl } from "@/lib/api/client";
import { getBatch, type Batch } from "@/lib/api/org";
import { currentMember, type DemoMember } from "@/lib/member";
import { formatAmountWithAsset } from "@/lib/money";
import { formatLagosDate } from "../../../account/bank/dates";
import { MemberSwitcher } from "../../MemberSwitcher";
import { stateLabel } from "../../states";
import { RunActions, type ActionOffer } from "./RunActions";

export const metadata: Metadata = { title: "Run · Waybill" };

const stampVariant: Record<Batch["state"], StampVariant> = {
  draft: "problem",
  ready: "waiting",
  awaiting_approval: "waiting",
  approved: "received",
  sending: "checking",
  confirming: "received",
  paid: "delivered",
  failed: "problem",
  rejected: "problem",
  cancelled: "waiting",
};

export default function RunPage({ params }: PageProps<"/org/batches/[id]">) {
  return (
    <main
      id="main"
      className="mx-auto flex w-full max-w-2xl flex-1 flex-col gap-6 px-4 py-8"
    >
      <p>
        <Link href="/org" className="underline">
          Organisation
        </Link>
      </p>
      <Suspense
        fallback={
          <div
            role="status"
            aria-busy="true"
            aria-label="Loading the run"
            className="h-96 bg-rule/40"
          />
        }
      >
        <Loaded params={params} />
      </Suspense>
      <p className="text-sm text-ink-muted">
        Test money only. Nothing here is real.
      </p>
    </main>
  );
}

// What the member the server acts as may try. The API decides; this only
// avoids offering buttons that are sure to be refused.
function offers(b: Batch, who: DemoMember): ActionOffer[] {
  const out: ActionOffer[] = [];
  const maker = who === "admin";
  if (who === "approver" && b.state === "awaiting_approval") {
    out.push({ action: "approve", label: "Approve the run" });
    out.push({ action: "reject", label: "Reject the run" });
  }
  if (
    maker &&
    (b.state === "approved" || (b.state === "ready" && !b.needsApproval))
  ) {
    out.push({
      action: "send",
      label: `Send ${formatAmountWithAsset(b.total)}`,
    });
  }
  if (
    maker &&
    ["draft", "ready", "awaiting_approval", "approved"].includes(b.state)
  ) {
    out.push({ action: "cancel", label: "Cancel the run" });
  }
  return out;
}

async function Loaded({ params }: { params: Promise<{ id: string }> }) {
  await connection();
  const { id } = await params;
  const { who, key } = await currentMember();
  if (!key) {
    return (
      <p role="alert">The demo organisation is not set up on this server.</p>
    );
  }
  const r = await getBatch(apiBaseUrl(), key, id);
  if (r.kind === "problem" && r.problem.status === 404) {
    return (
      <h1 className="text-2xl font-semibold">This run doesn&apos;t exist</h1>
    );
  }
  if (r.kind !== "ok") {
    return (
      <p role="alert" className="border-l-[3px] border-ink pl-3">
        We can&apos;t load this run right now. Try again in a minute.
      </p>
    );
  }
  const b = r.value;
  const moving = b.state === "sending" || b.state === "confirming";
  return (
    <>
      <header className="flex flex-col gap-3">
        <h1 className="text-2xl font-semibold">
          Run of{" "}
          <span className="font-mono">{formatAmountWithAsset(b.total)}</span>
        </h1>
        <div className="flex flex-wrap items-center gap-3">
          <Stamp
            variant={stampVariant[b.state]}
            label={stateLabel[b.state]}
            description={`Run state: ${stateLabel[b.state]}`}
          />
          <span data-testid="run-state" className="sr-only">
            {b.state}
          </span>
        </div>
        {b.needsApproval && b.state === "awaiting_approval" && (
          <p>
            This run is above the organisation&apos;s threshold. Someone other
            than its creator must approve it.
          </p>
        )}
        {b.problem && (
          <p className="border-l-[3px] border-ink pl-3">{b.problem}</p>
        )}
        {b.txHash && (
          <p className="text-sm">
            Transaction <span className="font-mono break-all">{b.txHash}</span>
          </p>
        )}
        {moving && (
          <p>
            The money is on its way.{" "}
            <a href="" className="underline">
              Refresh
            </a>{" "}
            to see the latest.
          </p>
        )}
      </header>

      <MemberSwitcher current={who} back={`/org/batches/${b.id}`} />
      <RunActions id={b.id} offers={offers(b, who)} />

      <section aria-labelledby="lines-heading" className="flex flex-col gap-3">
        <h2 id="lines-heading" className="text-xl font-semibold">
          Lines
        </h2>
        <ol className="flex flex-col gap-3">
          {(b.lines ?? []).map((l) => (
            <li
              key={l.lineNo}
              className="flex flex-col gap-1 border border-ink p-4"
            >
              <p className="font-semibold">
                {l.contractorName}
                {l.verified ? "" : " (not verified)"}
              </p>
              <p>
                <span className="font-mono">
                  {formatAmountWithAsset(l.amount)}
                </span>{" "}
                · {l.reference}
              </p>
              {l.walletAddress && (
                <p className="text-sm">
                  To{" "}
                  <span className="font-mono break-all">{l.walletAddress}</span>
                </p>
              )}
              {l.problem ? (
                <p className="font-semibold">! {l.problem}</p>
              ) : (
                <p className="text-sm">
                  <Link href={`/t/${l.trackingCode}`} className="underline">
                    Track this payment
                  </Link>
                </p>
              )}
            </li>
          ))}
        </ol>
      </section>

      {(b.approvals ?? []).length > 0 && (
        <section
          aria-labelledby="approvals-heading"
          className="flex flex-col gap-2"
        >
          <h2 id="approvals-heading" className="text-xl font-semibold">
            Decisions
          </h2>
          <ul>
            {(b.approvals ?? []).map((a) => (
              <li key={a.approverId}>
                {a.decision === "approved" ? "Approved" : "Rejected"}{" "}
                {formatLagosDate(a.decidedAt)}
              </li>
            ))}
          </ul>
        </section>
      )}
    </>
  );
}
