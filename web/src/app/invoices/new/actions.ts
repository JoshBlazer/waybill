"use server";

import { apiBaseUrl, createInvoice, type Invoice } from "@/lib/api/client";

export type CreateState =
  | { status: "idle" }
  | { status: "created"; invoice: Invoice }
  | { status: "error"; message: string; values: FormValues };

// What the user typed, returned with an error so the form can show it again:
// React resets a form after its action runs.
export type FormValues = { description: string; amount: string };

// Demo flow: the Next.js server creates invoices as the seeded demo
// contractor, using a key that only the server holds. Real contractor
// sign-in arrives with organisations in stage 4.
export async function createInvoiceAction(
  _prev: CreateState,
  form: FormData,
): Promise<CreateState> {
  const values: FormValues = {
    description: String(form.get("description") ?? ""),
    amount: String(form.get("amount") ?? "").trim(),
  };
  const key = process.env.WAYBILL_DEV_CONTRACTOR_KEY;
  if (!key) {
    return {
      status: "error",
      message: "Invoice creation is not set up on this server.",
      values,
    };
  }
  const idempotencyKey = String(form.get("idempotencyKey") ?? "");
  const result = await createInvoice(apiBaseUrl(), key, idempotencyKey, {
    description: values.description,
    amount: values.amount,
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
        values,
      };
    default:
      return {
        status: "error",
        // The request may have reached the API: the outcome is unknown.
        message:
          "We couldn't reach Waybill, so we don't know whether the invoice was created. Try again: it won't be created twice.",
        values,
      };
  }
}
