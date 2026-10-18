import type { Metadata } from "next";
import Link from "next/link";
import { connection } from "next/server";
import { Suspense } from "react";
import { currentMember } from "@/lib/member";
import { MemberSwitcher } from "../../MemberSwitcher";
import { NewRunForm } from "./NewRunForm";

export const metadata: Metadata = { title: "New run · Waybill" };

// The demo contractor's id, so a run can be built without looking it up.
const DEMO_CONTRACTOR = "00000000-0000-4000-8000-00000000d001";

export default function NewRunPage() {
  return (
    <main
      id="main"
      className="mx-auto flex w-full max-w-2xl flex-1 flex-col gap-6 px-4 py-8"
    >
      <header className="flex flex-col gap-1">
        <p>
          <Link href="/org" className="underline">
            Organisation
          </Link>
        </p>
        <h1 className="text-2xl font-semibold">New run</h1>
        <p>
          Pay several contractors in one transaction. Each line is checked
          before anything can be sent.
        </p>
      </header>
      <Suspense fallback={null}>
        <Loaded />
      </Suspense>
      <p className="text-sm text-ink-muted">
        Test money only. Nothing here is real.
      </p>
    </main>
  );
}

async function Loaded() {
  await connection();
  const { who } = await currentMember();
  return (
    <>
      <MemberSwitcher current={who} back="/org/batches/new" />
      <p className="text-sm">
        The demo contractor, Adaeze Okafor, has the id{" "}
        <span className="font-mono break-all">{DEMO_CONTRACTOR}</span>.
      </p>
      <NewRunForm idempotencyKey={crypto.randomUUID()} />
    </>
  );
}
