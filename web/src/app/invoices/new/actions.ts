"use server";

import { apiBaseUrl, createInvoice, type Invoice } from "@/lib/api/client";

export type CreateState =
  | { status: "idle" }
  | { status: "created"; invoice: Invoice }
  | { status: "error"; message: string };

// Demo flow: the Next.js server creates invoices as the seeded demo
// contractor, using a key that only the server holds. Real contractor
// sign-in arrives with organisations in stage 4.
export async function createInvoiceAction(
  _prev: CreateState,
  form: FormData,
): Promise<CreateState> {
  const key = process.env.WAYBILL_DEV_CONTRACTOR_KEY;
  if (!key) {
    return {
      status: "error",
      message: "Invoice creation is not set up on this server.",
    };
  }
  const idempotencyKey = String(form.get("idempotencyKey") ?? "");
  const result = await createInvoice(apiBaseUrl(), key, idempotencyKey, {
    description: String(form.get("description") ?? ""),
    amount: String(form.get("amount") ?? "").trim(),
    asset: "USDC",
    payout: "hold",
  });
  switch (result.kind) {
    case "created":
      return { status: "created", invoice: result.invoice };
    case "problem":
      return {
        status: "error",
        message: result.problem.detail ?? result.problem.title,
      };
    default:
      return {
        status: "error",
        message: "We couldn't reach Waybill. Nothing was created; try again.",
      };
  }
}
