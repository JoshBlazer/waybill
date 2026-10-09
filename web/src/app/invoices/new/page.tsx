import type { Metadata } from "next";
import { connection } from "next/server";
import { Suspense } from "react";
import { CreateInvoiceForm } from "./CreateInvoiceForm";

export const metadata: Metadata = { title: "New invoice · Waybill" };

export default function NewInvoicePage() {
  return (
    <main
      id="main"
      className="mx-auto flex w-full max-w-xl flex-1 flex-col gap-6 px-4 py-8"
    >
      <header className="flex flex-col gap-1">
        <p className="text-sm text-ink-muted">Demo contractor: Adaeze Okafor</p>
        <h1 className="text-2xl font-semibold">New invoice</h1>
      </header>
      <Suspense fallback={<div aria-busy="true" className="h-64 bg-rule/40" />}>
        <FreshForm />
      </Suspense>
      <p className="text-sm text-ink-muted">
        Test money only. Nothing here is real.
      </p>
    </main>
  );
}

// A fresh idempotency key per page load, generated on the server so the form
// works before (or without) JavaScript.
async function FreshForm() {
  await connection();
  return <CreateInvoiceForm idempotencyKey={crypto.randomUUID()} />;
}
