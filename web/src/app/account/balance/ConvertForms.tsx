"use client";

import Link from "next/link";
import { useActionState, useEffect, useRef, useState } from "react";
import {
  acceptAction,
  quoteAction,
  type AcceptState,
  type ConvertState,
} from "./actions";

function ErrorMessage({ message }: { message: string }) {
  const ref = useRef<HTMLParagraphElement>(null);
  useEffect(() => ref.current?.focus(), [message]);
  return (
    <p
      ref={ref}
      tabIndex={-1}
      role="alert"
      className="border-l-[3px] border-ink pl-3 font-semibold"
    >
      ! {message}
    </p>
  );
}

export function ConvertForm() {
  const [state, action, pending] = useActionState<ConvertState, FormData>(
    quoteAction,
    { status: "idle" },
  );
  return (
    <form action={action} className="flex flex-col gap-4">
      {state.status === "error" && <ErrorMessage message={state.message} />}
      <div className="flex flex-col gap-1">
        <label htmlFor="amount" className="font-semibold">
          USDC to convert
        </label>
        <p id="amount-hint" className="text-sm text-ink-muted">
          At least 1 USDC. You see the rate and the naira before anything moves.
        </p>
        <input
          id="amount"
          name="amount"
          required
          inputMode="decimal"
          pattern="^[0-9]+(\.[0-9]{1,6})?$"
          aria-describedby="amount-hint"
          defaultValue={state.status === "error" ? state.amount : ""}
          autoComplete="off"
          className="min-h-11 max-w-xs border border-ink bg-paper-raised px-3 font-mono"
        />
      </div>
      <button
        type="submit"
        disabled={pending}
        className="min-h-11 self-start border-2 border-ink bg-ink px-5 font-semibold text-paper disabled:opacity-60"
      >
        {pending ? "Getting a rate…" : "Get a rate"}
      </button>
    </form>
  );
}

/** Seconds left on the quote, counted down in the browser. */
function useSecondsLeft(expiresAt: string): number {
  const [left, setLeft] = useState(() =>
    Math.max(0, Math.floor((Date.parse(expiresAt) - Date.now()) / 1000)),
  );
  useEffect(() => {
    const t = setInterval(() => {
      setLeft(
        Math.max(0, Math.floor((Date.parse(expiresAt) - Date.now()) / 1000)),
      );
    }, 1000);
    return () => clearInterval(t);
  }, [expiresAt]);
  return left;
}

export function AcceptForm({
  id,
  expiresAt,
  label,
}: {
  id: string;
  expiresAt: string;
  label: string;
}) {
  const [state, action, pending] = useActionState<AcceptState, FormData>(
    acceptAction,
    { status: "idle" },
  );
  const left = useSecondsLeft(expiresAt);
  if (left === 0) {
    return (
      <p role="status">
        This rate has expired, so nothing was converted.{" "}
        <Link href="/account/balance" className="font-semibold underline">
          Get a new rate
        </Link>
      </p>
    );
  }
  const mm = Math.floor(left / 60);
  const ss = String(left % 60).padStart(2, "0");
  return (
    <form action={action} className="flex flex-col gap-3">
      {state.status === "error" && <ErrorMessage message={state.message} />}
      <input type="hidden" name="id" value={id} />
      <p className="text-sm" aria-live="off">
        This rate holds for{" "}
        <span className="font-mono" data-testid="quote-countdown">
          {mm}:{ss}
        </span>
        .
      </p>
      <button
        type="submit"
        disabled={pending}
        className="min-h-11 self-start border-2 border-ink bg-ink px-5 font-semibold text-paper disabled:opacity-60"
      >
        {pending ? "Converting…" : label}
      </button>
    </form>
  );
}
