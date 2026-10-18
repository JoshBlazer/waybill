"use server";

import { cookies } from "next/headers";
import { redirect } from "next/navigation";
import { apiBaseUrl } from "@/lib/api/client";
import {
  batchAction,
  createBatch,
  type BatchAction,
  type NewLine,
} from "@/lib/api/org";
import { currentMember, MEMBER_COOKIE } from "@/lib/member";
import { LINE_ROWS } from "./states";

/** Switches which demo member the server acts as. */
export async function switchMemberAction(form: FormData) {
  const who = form.get("member") === "approver" ? "approver" : "admin";
  const jar = await cookies();
  jar.set(MEMBER_COOKIE, who, {
    httpOnly: true,
    sameSite: "lax",
    path: "/",
  });
  const back = String(form.get("back") ?? "/org");
  redirect(back.startsWith("/org") ? back : "/org");
}

export type RunFormState =
  { status: "idle" } | { status: "error"; message: string; lines: NewLine[] };

/** Builds a run from the form's filled rows and opens it. */
export async function createRunAction(
  _prev: RunFormState,
  form: FormData,
): Promise<RunFormState> {
  const lines: NewLine[] = [];
  for (let i = 0; i < LINE_ROWS; i++) {
    const line = {
      contractorId: String(form.get(`contractorId${i}`) ?? "").trim(),
      amount: String(form.get(`amount${i}`) ?? "").trim(),
      reference: String(form.get(`reference${i}`) ?? "").trim(),
    };
    if (line.contractorId || line.amount || line.reference) lines.push(line);
  }
  if (lines.length === 0) {
    return { status: "error", message: "Add at least one line.", lines };
  }
  const { key } = await currentMember();
  if (!key) {
    return {
      status: "error",
      message: "The demo organisation is not set up on this server.",
      lines,
    };
  }
  const result = await createBatch(
    apiBaseUrl(),
    key,
    String(form.get("idempotencyKey") ?? ""),
    lines,
  );
  if (result.kind === "ok") redirect(`/org/batches/${result.value.id}`);
  if (result.kind === "problem") {
    return {
      status: "error",
      message: result.problem.detail
        ? `${result.problem.title} ${result.problem.detail}`
        : result.problem.title,
      lines,
    };
  }
  return {
    status: "error",
    // Building a run moves no money, and a retry with the same form is
    // recognised: it is safe to try again.
    message:
      "We couldn't reach Waybill. Try again: the run won't be built twice.",
    lines,
  };
}

export type RunActionState =
  { status: "idle" } | { status: "error"; message: string };

const actions: BatchAction[] = ["approve", "reject", "send", "cancel"];

/** Approves, rejects, sends or cancels a run, then shows it again. */
export async function runAction(
  _prev: RunActionState,
  form: FormData,
): Promise<RunActionState> {
  const id = String(form.get("id") ?? "");
  const action = String(form.get("action") ?? "") as BatchAction;
  if (!actions.includes(action)) {
    return { status: "error", message: "Unknown action." };
  }
  const { key } = await currentMember();
  if (!key) {
    return {
      status: "error",
      message: "The demo organisation is not set up on this server.",
    };
  }
  const result = await batchAction(apiBaseUrl(), key, id, action);
  if (result.kind === "ok") redirect(`/org/batches/${id}`);
  if (result.kind === "problem") {
    return {
      status: "error",
      message: result.problem.detail
        ? `${result.problem.title} ${result.problem.detail}`
        : result.problem.title,
    };
  }
  return {
    status: "error",
    message:
      "We couldn't reach Waybill, so we don't know whether that worked. Reload the page to see the run's state before trying again.",
  };
}
