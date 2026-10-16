"use client";

import { useActionState, useEffect, useRef } from "react";
import type { Bank, BankAccount } from "@/lib/api/client";
import { saveBankAccountAction, type SaveState } from "./actions";
import { formatLagosDate } from "./dates";

export function BankAccountForm({
  banks,
  current,
  now,
}: {
  banks: Bank[];
  current: BankAccount | null;
  now: string; // server time of the page request
}) {
  const [state, action, pending] = useActionState<SaveState, FormData>(
    saveBankAccountAction,
    { status: "idle" },
  );
  const messageRef = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    if (state.status !== "idle") messageRef.current?.focus();
  }, [state]);

  const shown = state.status === "saved" ? state.account : current;
  const checkedAt = state.status === "saved" ? state.checkedAt : now;
  const values = state.status === "error" ? state.values : undefined;

  return (
    <div className="flex flex-col gap-6">
      {shown && <CurrentAccount account={shown} now={checkedAt} />}

      <form action={action} className="flex flex-col gap-5">
        <h2 className="text-xl font-semibold">
          {shown ? "Change account" : "Add your account"}
        </h2>
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
        {state.status === "saved" && (
          <p
            ref={messageRef}
            tabIndex={-1}
            role="status"
            className="border-l-[3px] border-ink pl-3 font-semibold"
          >
            Saved. The bank holds this account as {state.account.accountName}.
          </p>
        )}
        <div className="flex flex-col gap-1">
          <label htmlFor="bankCode" className="font-semibold">
            Bank
          </label>
          <select
            id="bankCode"
            name="bankCode"
            required
            defaultValue={values?.bankCode ?? shown?.bankCode ?? ""}
            className="min-h-11 border border-ink bg-paper-raised px-3"
          >
            <option value="" disabled>
              Choose your bank
            </option>
            {banks.map((b) => (
              <option key={b.code} value={b.code}>
                {b.name}
              </option>
            ))}
          </select>
        </div>
        <div className="flex flex-col gap-1">
          <label htmlFor="accountNumber" className="font-semibold">
            Account number
          </label>
          <p id="account-hint" className="text-sm text-ink-muted">
            10 digits. We check with the bank that the account is in your name.
          </p>
          <input
            id="accountNumber"
            name="accountNumber"
            required
            inputMode="numeric"
            autoComplete="off"
            pattern="^[0-9]{10}$"
            maxLength={10}
            aria-describedby="account-hint"
            defaultValue={values?.accountNumber}
            className="min-h-11 border border-ink bg-paper-raised px-3 font-mono"
          />
        </div>
        <button
          type="submit"
          disabled={pending}
          className="min-h-11 self-start border-2 border-ink bg-ink px-5 font-semibold text-paper disabled:opacity-60"
        >
          {pending ? "Checking with the bank…" : "Check and save"}
        </button>
      </form>
    </div>
  );
}

function CurrentAccount({
  account,
  now,
}: {
  account: BankAccount;
  now: string;
}) {
  const waiting =
    new Date(account.payoutsAllowedFrom).getTime() > new Date(now).getTime();
  return (
    <section
      aria-labelledby="current-heading"
      className="perforated flex flex-col gap-2 px-5 py-6"
    >
      <h2 id="current-heading" className="text-xl font-semibold">
        Naira goes to
      </h2>
      <p className="text-lg">
        {account.bankName}{" "}
        <span className="font-mono">
          <span aria-hidden="true">••••••</span>
          <span className="sr-only">account ending </span>
          {account.accountNumberLast4}
        </span>
      </p>
      <p>Name at the bank: {account.accountName}</p>
      {waiting ? (
        <p>
          Payouts to this account start{" "}
          {formatLagosDate(account.payoutsAllowedFrom)}. We wait 24 hours after
          a change, so a stolen key can&apos;t send your money elsewhere.
        </p>
      ) : (
        <p className="text-ink-muted">Ready for payouts.</p>
      )}
    </section>
  );
}
