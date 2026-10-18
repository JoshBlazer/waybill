"use server";

import { redirect } from "next/navigation";
import { apiBaseUrl } from "@/lib/api/client";
import { acceptConversion, quoteConversion } from "@/lib/api/org";

export type ConvertState =
  { status: "idle" } | { status: "error"; message: string; amount: string };

// Demo flow, as for the bank account: the server acts as the demo
// contractor with a key only it holds.
export async function quoteAction(
  _prev: ConvertState,
  form: FormData,
): Promise<ConvertState> {
  const amount = String(form.get("amount") ?? "").trim();
  const key = process.env.WAYBILL_DEV_CONTRACTOR_KEY;
  if (!key) {
    return {
      status: "error",
      message: "Conversions are not set up on this server.",
      amount,
    };
  }
  const r = await quoteConversion(apiBaseUrl(), key, amount);
  if (r.kind === "ok") redirect(`/account/balance?quote=${r.value.id}`);
  if (r.kind === "problem") {
    return { status: "error", message: r.problem.title, amount };
  }
  return {
    status: "error",
    message: "We couldn't reach Waybill. Nothing was converted; try again.",
    amount,
  };
}

export type AcceptState =
  { status: "idle" } | { status: "error"; message: string };

export async function acceptAction(
  _prev: AcceptState,
  form: FormData,
): Promise<AcceptState> {
  const id = String(form.get("id") ?? "");
  const key = process.env.WAYBILL_DEV_CONTRACTOR_KEY;
  if (!key) {
    return {
      status: "error",
      message: "Conversions are not set up on this server.",
    };
  }
  const r = await acceptConversion(apiBaseUrl(), key, id);
  if (r.kind === "ok") redirect(`/account/balance?accepted=${r.value.id}`);
  if (r.kind === "problem") {
    return { status: "error", message: r.problem.title };
  }
  return {
    status: "error",
    // The idempotency key is fixed per conversion, so a retry is safe.
    message:
      "We couldn't reach Waybill. Try again: accepting twice converts once.",
  };
}
