"use client";

import { useActionState, useEffect, useRef } from "react";
import type { BatchAction } from "@/lib/api/org";
import { runAction, type RunActionState } from "../../actions";

export type ActionOffer = { action: BatchAction; label: string };

export function RunActions({
  id,
  offers,
}: {
  id: string;
  offers: ActionOffer[];
}) {
  const [state, action, pending] = useActionState<RunActionState, FormData>(
    runAction,
    { status: "idle" },
  );
  const messageRef = useRef<HTMLParagraphElement>(null);
  useEffect(() => {
    if (state.status === "error") messageRef.current?.focus();
  }, [state]);
  if (offers.length === 0 && state.status === "idle") return null;
  return (
    <section aria-label="Actions" className="flex flex-col gap-3">
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
      <div className="flex flex-wrap gap-3">
        {offers.map((o) => (
          <form key={o.action} action={action}>
            <input type="hidden" name="id" value={id} />
            <input type="hidden" name="action" value={o.action} />
            <button
              type="submit"
              disabled={pending}
              className={
                o.action === "send" || o.action === "approve"
                  ? "min-h-11 border-2 border-ink bg-ink px-5 font-semibold text-paper disabled:opacity-60"
                  : "min-h-11 border border-ink px-4 font-semibold disabled:opacity-60"
              }
            >
              {o.label}
            </button>
          </form>
        ))}
      </div>
    </section>
  );
}
