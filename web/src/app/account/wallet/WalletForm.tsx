"use client";

import { useActionState, useEffect, useRef } from "react";
import { saveWalletAction, type WalletState } from "./actions";

export function WalletForm({ network }: { network: string }) {
  const [state, action, pending] = useActionState<WalletState, FormData>(
    saveWalletAction,
    { status: "idle" },
  );
  const messageRef = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    if (state.status === "error") messageRef.current?.focus();
  }, [state]);
  return (
    <form action={action} className="flex flex-col gap-4">
      <h2 className="text-xl font-semibold">Set a wallet</h2>
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
      <input type="hidden" name="network" value={network} />
      <div className="flex flex-col gap-1">
        <label htmlFor="address" className="font-semibold">
          Wallet address on {network}
        </label>
        <p id="address-hint" className="text-sm text-ink-muted">
          A new or changed wallet can be paid 24 hours after you save it, so a
          stolen key can&apos;t redirect your money at once.
        </p>
        <input
          id="address"
          name="address"
          required
          pattern="^0x[0-9a-fA-F]{40}$"
          aria-describedby="address-hint"
          defaultValue={state.status === "error" ? state.address : ""}
          autoComplete="off"
          spellCheck={false}
          className="min-h-11 border border-ink bg-paper-raised px-3 font-mono text-sm"
        />
      </div>
      <button
        type="submit"
        disabled={pending}
        className="min-h-11 self-start border-2 border-ink bg-ink px-5 font-semibold text-paper disabled:opacity-60"
      >
        {pending ? "Saving…" : "Save wallet"}
      </button>
    </form>
  );
}
