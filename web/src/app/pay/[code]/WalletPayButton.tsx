"use client";

import dynamic from "next/dynamic";
import { useState } from "react";
import type { WalletPayProps } from "./WalletPay";

// The wallet libraries load only after the payer chooses to use a wallet.
const WalletPay = dynamic(() => import("./WalletPay"), {
  ssr: false,
  loading: () => <p aria-busy="true">Loading wallet support…</p>,
});

export function WalletPayButton(props: WalletPayProps) {
  const [open, setOpen] = useState(false);
  if (open) return <WalletPay {...props} />;
  return (
    <button
      type="button"
      className="min-h-11 self-start border border-ink px-4 font-semibold"
      onClick={() => setOpen(true)}
    >
      Pay with a browser wallet
    </button>
  );
}
