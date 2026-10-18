"use server";

import { redirect } from "next/navigation";
import { apiBaseUrl, startCheckout } from "@/lib/api/client";

export type CheckoutState =
  { status: "idle" } | { status: "error"; message: string };

// Sends the payer to Paystack's test checkout for this link. Starting it
// twice returns the same open checkout, so a retry is always safe.
export async function startCheckoutAction(
  _prev: CheckoutState,
  form: FormData,
): Promise<CheckoutState> {
  const code = String(form.get("code") ?? "");
  const result = await startCheckout(apiBaseUrl(), code);
  if (result.kind === "started") {
    const url = new URL(result.checkout.authorizationUrl);
    if (url.protocol !== "https:" || !url.hostname.endsWith(".paystack.com")) {
      return {
        status: "error",
        message:
          "The payment page address looks wrong, so we stopped. Pay in stablecoin instead.",
      };
    }
    redirect(url.toString());
  }
  if (result.kind === "problem") {
    return { status: "error", message: result.problem.title };
  }
  return {
    status: "error",
    message: "We couldn't reach Waybill. Nothing was charged. Try again.",
  };
}
