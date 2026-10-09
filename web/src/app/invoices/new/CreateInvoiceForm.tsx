"use client";

import Link from "next/link";
import { useActionState, useEffect, useRef } from "react";
import { formatAmountWithAsset } from "@/lib/money";
import { createInvoiceAction, type CreateState } from "./actions";

export function CreateInvoiceForm({
  idempotencyKey,
}: {
  idempotencyKey: string;
}) {
  const [state, action, pending] = useActionState<CreateState, FormData>(
    createInvoiceAction,
    { status: "idle" },
  );
  // The key comes from the server, one per page load, and is never rotated:
  // a double submit or a retry after a timeout (when the invoice may already
  // exist) replays the original; after a validation error the server has
  // freed the key, so reusing it with corrected input is a fresh request.
  const resultRef = useRef<HTMLHeadingElement>(null);
  const errorRef = useRef<HTMLParagraphElement>(null);

  useEffect(() => {
    if (state.status === "created") resultRef.current?.focus();
    if (state.status === "error") errorRef.current?.focus();
  }, [state]);

  if (state.status === "created") {
    const inv = state.invoice;
    return (
      <section
        aria-labelledby="created-heading"
        className="perforated flex flex-col gap-4 px-5 py-7"
      >
        <h2
          id="created-heading"
          ref={resultRef}
          tabIndex={-1}
          className="text-xl font-semibold"
        >
          Invoice created
        </h2>
        <p className="font-mono text-2xl">
          {formatAmountWithAsset(inv.amount)}
        </p>
        <p>
          Tracking number <span className="font-mono">{inv.trackingCode}</span>
        </p>
        <p>Send this payment link to the payer:</p>
        <p className="font-mono text-sm break-all">{inv.payUrl}</p>
        <div className="flex flex-wrap gap-4">
          <Link href={`/pay/${inv.trackingCode}`} className="underline">
            Open payment link
          </Link>
          <Link href={`/t/${inv.trackingCode}`} className="underline">
            Track it
          </Link>
          <button
            type="button"
            className="underline"
            onClick={() => window.location.reload()}
          >
            Create another
          </button>
        </div>
      </section>
    );
  }

  return (
    <form action={action} className="flex flex-col gap-5" noValidate={false}>
      <input type="hidden" name="idempotencyKey" value={idempotencyKey} />
      {state.status === "error" && (
        <p
          ref={errorRef}
          tabIndex={-1}
          role="alert"
          className="border-l-[3px] border-ink pl-3 font-semibold"
        >
          ! {state.message}
        </p>
      )}
      <div className="flex flex-col gap-1">
        <label htmlFor="description" className="font-semibold">
          What is it for?
        </label>
        <input
          id="description"
          name="description"
          required
          maxLength={500}
          className="min-h-11 border border-ink bg-paper-raised px-3"
          placeholder=""
        />
      </div>
      <div className="flex flex-col gap-1">
        <label htmlFor="amount" className="font-semibold">
          Amount in USDC
        </label>
        <p id="amount-hint" className="text-sm text-ink-muted">
          For example 125.50. Up to 6 decimal places.
        </p>
        <input
          id="amount"
          name="amount"
          required
          inputMode="decimal"
          pattern="^(0|[1-9][0-9]*)(\.[0-9]{1,6})?$"
          aria-describedby="amount-hint"
          className="min-h-11 border border-ink bg-paper-raised px-3 font-mono"
        />
      </div>
      <p className="text-sm text-ink-muted">
        You&apos;ll keep the USDC in your Waybill balance. Naira to your bank
        arrives in a later version.
      </p>
      <button
        type="submit"
        disabled={pending}
        className="min-h-11 self-start border-2 border-ink bg-ink px-5 font-semibold text-paper disabled:opacity-60"
      >
        {pending ? "Creating…" : "Create invoice"}
      </button>
    </form>
  );
}
