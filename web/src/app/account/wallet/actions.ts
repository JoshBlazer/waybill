"use server";

import { redirect } from "next/navigation";
import { apiBaseUrl } from "@/lib/api/client";
import { setPayoutWallet } from "@/lib/api/org";

export type WalletState =
  { status: "idle" } | { status: "error"; message: string; address: string };

// Demo flow, as for the bank account: the server acts as the demo
// contractor with a key only it holds.
export async function saveWalletAction(
  _prev: WalletState,
  form: FormData,
): Promise<WalletState> {
  const address = String(form.get("address") ?? "").trim();
  const network = String(form.get("network") ?? "");
  const key = process.env.WAYBILL_DEV_CONTRACTOR_KEY;
  if (!key) {
    return {
      status: "error",
      message: "Wallets are not set up on this server.",
      address,
    };
  }
  const r = await setPayoutWallet(apiBaseUrl(), key, network, address);
  if (r.kind === "ok") redirect("/account/wallet");
  if (r.kind === "problem") {
    return {
      status: "error",
      message: r.problem.detail
        ? `${r.problem.title} ${r.problem.detail}`
        : r.problem.title,
      address,
    };
  }
  return {
    status: "error",
    // Saving is safe to repeat: the same address changes nothing.
    message:
      "We couldn't reach Waybill. Try again: saving it twice changes nothing.",
    address,
  };
}
