import { connection } from "next/server";

// A plain GET form: the browser downloads the CSV from a same-origin route
// that adds the demo key. No JavaScript needed. The default range (this
// month so far) depends on today, so it renders at request time.

function iso(d: Date): string {
  return d.toISOString().slice(0, 10);
}

export async function ExportForm({ action }: { action: string }) {
  await connection();
  const now = new Date();
  const first = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1));
  const tomorrow = new Date(now.getTime() + 24 * 60 * 60 * 1000);
  return (
    <section
      aria-labelledby="export-heading"
      className="flex flex-col gap-3 print:hidden"
    >
      <h2 id="export-heading" className="text-xl font-semibold">
        Export for your accountant
      </h2>
      <p className="text-sm text-ink-muted">
        Every movement on your balances as CSV: money in, money out, the balance
        after each, and the transaction or provider reference. Days are in UTC;
        the end day is not included.
      </p>
      <form
        action={action}
        method="get"
        className="flex flex-wrap items-end gap-4"
      >
        <div className="flex flex-col gap-1">
          <label htmlFor="export-from" className="font-semibold">
            From
          </label>
          <input
            id="export-from"
            name="from"
            type="date"
            required
            defaultValue={iso(first)}
            className="min-h-11 border border-ink bg-paper-raised px-3 font-mono text-sm"
          />
        </div>
        <div className="flex flex-col gap-1">
          <label htmlFor="export-to" className="font-semibold">
            Up to (not including)
          </label>
          <input
            id="export-to"
            name="to"
            type="date"
            required
            defaultValue={iso(tomorrow)}
            className="min-h-11 border border-ink bg-paper-raised px-3 font-mono text-sm"
          />
        </div>
        <button
          type="submit"
          className="min-h-11 border-2 border-ink px-5 font-semibold"
        >
          Download CSV
        </button>
      </form>
    </section>
  );
}
