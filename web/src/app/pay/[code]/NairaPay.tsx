"use client";

import { useActionState, useEffect, useRef } from "react";
import { formatAmount, type Amount } from "@/lib/money";
import { startCheckoutAction, type CheckoutState } from "./actions";

// Pay the link in naira through Paystack's test checkout. Shown only when
// the API says the link can be paid this way right now.
export function NairaPay({
  code,
  amount,
  until,
}: {
  code: string;
  amount: Amount;
  until: string; // already formatted, Lagos time
}) {
  const [state, action, pending] = useActionState<CheckoutState, FormData>(
    startCheckoutAction,
    { status: "idle" },
  );
  const messageRef = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    if (state.status === "error") messageRef.current?.focus();
  }, [state]);
  return (
    <section aria-labelledby="naira-heading" className="flex flex-col gap-3">
      <h2 id="naira-heading" className="text-xl font-semibold">
        Pay in naira
      </h2>
      <p>
        Pay <span className="font-mono">₦{formatAmount(amount)}</span> by card
        or bank transfer. This amount holds until {until}.
      </p>
      {state.status === "error" && (
        <p
          ref={messageRef}
          tabIndex={-1}
          role="alert"
          className="border-l-[3px] border-ink pl-3 font-semibold"
        >
          ! {state.message}
        </p>
      )}
      <form action={action}>
        <input type="hidden" name="code" value={code} />
        <button
          type="submit"
          disabled={pending}
          className="min-h-11 border border-ink px-4 font-semibold disabled:opacity-60"
        >
          {pending
            ? "Opening the payment page…"
            : "Pay by card or bank transfer"}
        </button>
      </form>
      <p className="text-sm text-ink-muted">
        You&apos;ll go to Paystack&apos;s test checkout, then come back to the
        tracking page.
      </p>
    </section>
  );
}
