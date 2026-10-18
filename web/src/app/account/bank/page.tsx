import type { Metadata } from "next";
import Link from "next/link";
import { connection } from "next/server";
import { Suspense } from "react";
import { ExportForm } from "@/components/ExportForm";
import { apiBaseUrl, fetchBankAccount, fetchBanks } from "@/lib/api/client";
import { BankAccountForm } from "./BankAccountForm";

export const metadata: Metadata = { title: "Bank account · Waybill" };

export default function BankAccountPage() {
  return (
    <main
      id="main"
      className="mx-auto flex w-full max-w-xl flex-1 flex-col gap-6 px-4 py-8"
    >
      <header className="flex flex-col gap-1">
        <p className="text-sm text-ink-muted">Demo contractor: Adaeze Okafor</p>
        <h1 className="text-2xl font-semibold">Bank account</h1>
        <p>Where your naira is sent when an invoice is paid.</p>
      </header>
      <Suspense
        fallback={
          <div
            role="status"
            aria-busy="true"
            aria-label="Loading your bank account"
            className="h-64 bg-rule/40"
          />
        }
      >
        <Loaded />
      </Suspense>
      <Suspense fallback={null}>
        <ExportForm action="/account/export" />
      </Suspense>
      <p>
        <Link href="/invoices/new" className="underline">
          New invoice
        </Link>
      </p>
      <p className="text-sm text-ink-muted">
        Test money only. Nothing here is real.
      </p>
    </main>
  );
}

async function Loaded() {
  await connection(); // read at request time, never at build time
  const key = process.env.WAYBILL_DEV_CONTRACTOR_KEY;
  if (!key) {
    return <p role="alert">Bank accounts are not set up on this server.</p>;
  }
  const [banks, account] = await Promise.all([
    fetchBanks(apiBaseUrl(), key),
    fetchBankAccount(apiBaseUrl(), key),
  ]);
  if (banks.kind !== "ok" || account.kind === "unreachable") {
    return (
      <p role="alert" className="border-l-[3px] border-ink pl-3">
        We can&apos;t load your bank details right now. Try again in a minute.
      </p>
    );
  }
  return (
    <BankAccountForm
      banks={banks.value}
      current={account.kind === "ok" ? account.value : null}
      now={new Date().toISOString()}
    />
  );
}
