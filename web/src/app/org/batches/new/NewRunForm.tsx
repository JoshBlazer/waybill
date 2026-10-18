"use client";

import { useActionState, useEffect, useRef } from "react";
import { createRunAction, type RunFormState } from "../../actions";
import { LINE_ROWS } from "../../states";

export function NewRunForm({ idempotencyKey }: { idempotencyKey: string }) {
  const [state, action, pending] = useActionState<RunFormState, FormData>(
    createRunAction,
    { status: "idle" },
  );
  const messageRef = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    if (state.status === "error") messageRef.current?.focus();
  }, [state]);
  const values = state.status === "error" ? state.lines : [];
  return (
    <form action={action} className="flex flex-col gap-5">
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
      <input type="hidden" name="idempotencyKey" value={idempotencyKey} />
      {Array.from({ length: LINE_ROWS }, (_, i) => (
        <fieldset key={i} className="flex flex-col gap-3 border border-ink p-4">
          <legend className="px-1 font-semibold">Line {i + 1}</legend>
          <div className="flex flex-col gap-1">
            <label htmlFor={`contractorId${i}`}>Contractor id</label>
            <input
              id={`contractorId${i}`}
              name={`contractorId${i}`}
              defaultValue={values[i]?.contractorId ?? ""}
              autoComplete="off"
              spellCheck={false}
              className="min-h-11 border border-ink bg-paper-raised px-3 font-mono text-sm"
            />
          </div>
          <div className="flex flex-col gap-1">
            <label htmlFor={`amount${i}`}>Amount in USDC</label>
            <input
              id={`amount${i}`}
              name={`amount${i}`}
              inputMode="decimal"
              defaultValue={values[i]?.amount ?? ""}
              autoComplete="off"
              className="min-h-11 border border-ink bg-paper-raised px-3 font-mono"
            />
          </div>
          <div className="flex flex-col gap-1">
            <label htmlFor={`reference${i}`}>Reference</label>
            <input
              id={`reference${i}`}
              name={`reference${i}`}
              maxLength={140}
              defaultValue={values[i]?.reference ?? ""}
              className="min-h-11 border border-ink bg-paper-raised px-3"
            />
          </div>
        </fieldset>
      ))}
      <p className="text-sm text-ink-muted">Leave unused lines empty.</p>
      <button
        type="submit"
        disabled={pending}
        className="min-h-11 self-start bg-ink px-4 font-semibold text-paper disabled:opacity-60"
      >
        {pending ? "Checking the lines…" : "Build and check the run"}
      </button>
    </form>
  );
}
