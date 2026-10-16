"use server";

import {
  apiBaseUrl,
  saveBankAccount,
  type BankAccount,
} from "@/lib/api/client";

export type SaveState =
  | { status: "idle" }
  | { status: "saved"; account: BankAccount; checkedAt: string }
  | { status: "error"; message: string; values: BankValues };

export type BankValues = { bankCode: string; accountNumber: string };

// Demo flow, as for invoices: the Next.js server acts as the seeded demo
// contractor with a key only the server holds.
export async function saveBankAccountAction(
  _prev: SaveState,
  form: FormData,
): Promise<SaveState> {
  const values: BankValues = {
    bankCode: String(form.get("bankCode") ?? ""),
    accountNumber: String(form.get("accountNumber") ?? "").replace(/\s/g, ""),
  };
  const key = process.env.WAYBILL_DEV_CONTRACTOR_KEY;
  if (!key) {
    return {
      status: "error",
      message: "Bank accounts are not set up on this server.",
      values,
    };
  }
  const result = await saveBankAccount(
    apiBaseUrl(),
    key,
    values.bankCode,
    values.accountNumber,
  );
  switch (result.kind) {
    case "saved":
      return {
        status: "saved",
        account: result.account,
        checkedAt: new Date().toISOString(),
      };
    case "problem":
      return {
        status: "error",
        message: result.problem.detail
          ? `${result.problem.title} ${result.problem.detail}`
          : result.problem.title,
        values,
      };
    default:
      return {
        status: "error",
        // Saving is safe to repeat, so the honest advice is to try again.
        message:
          "We couldn't reach Waybill, so we don't know whether the account was saved. Try again: saving it twice changes nothing.",
        values,
      };
  }
}
