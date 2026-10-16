"use client";

import Link from "next/link";
import { useActionState, useEffect, useRef } from "react";
import type { BankAccount } from "@/lib/api/client";
import { formatAmount, formatAmountWithAsset } from "@/lib/money";
import { formatLagosDate } from "../../account/bank/dates";
import { createInvoiceAction, type CreateState } from "./actions";

/** The demo contractor's payout account: null when none is saved,
 *  "unknown" when it could not be loaded. */
export type PayoutAccount = BankAccount | null | "unknown";

export function CreateInvoiceForm({
  idempotencyKey,
  account,
}: {
  idempotencyKey: string;
  account: PayoutAccount;
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

  const values = state.status === "error" ? state.values : undefined;

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
        {inv.quote && (
          <p>
            You&apos;ll receive{" "}
            <span className="font-mono font-semibold">
              ₦{formatAmount(inv.quote.nairaAmount)}
            </span>{" "}
            at ₦{formatAmount(inv.quote.rate)} per USDC, locked{" "}
            {formatLagosDate(inv.quote.expiresAt).replace(/^on /, "until ")}.
            Rate from {inv.quote.source}.
          </p>
        )}
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
          defaultValue={values?.description}
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
          defaultValue={values?.amount}
          className="min-h-11 border border-ink bg-paper-raised px-3 font-mono"
        />
      </div>
      <fieldset className="flex flex-col gap-2">
        <legend className="font-semibold">How do you want to be paid?</legend>
        <label className="flex min-h-11 items-center gap-3">
          <input
            type="radio"
            name="payout"
            value="hold"
            defaultChecked={values?.payout !== "naira"}
            className="size-5"
          />
          Keep the USDC in my Waybill balance
        </label>
        <label className="flex min-h-11 items-center gap-3">
          <input
            type="radio"
            name="payout"
            value="naira"
            disabled={account === null}
            defaultChecked={values?.payout === "naira"}
            aria-describedby="naira-hint"
            className="size-5"
          />
          Naira to my bank, at a rate locked now
        </label>
        <p id="naira-hint" className="text-sm text-ink-muted">
          {account === null ? (
            <>
              <Link href="/account/bank" className="underline">
                Add a bank account
              </Link>{" "}
              to be paid in naira.
            </>
          ) : account === "unknown" ? (
            "We couldn't check your bank account just now; naira will work if one is saved."
          ) : (
            `Sent to ${account.bankName}, account ending ${account.accountNumberLast4}.`
          )}
        </p>
      </fieldset>
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
