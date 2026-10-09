// A status shown as a rubber-stamp mark (docs/DESIGN.md §3). The label is
// real text; `description` is the full sentence read by screen readers.
export type StampVariant =
  "received" | "confirmed" | "delivered" | "waiting" | "checking" | "problem";

const variantClass: Record<StampVariant, string> = {
  received: "border-ink text-ink",
  confirmed: "border-ink text-ink border-double border-[3px]",
  delivered:
    "border-delivered text-paper bg-delivered border-double border-[3px]",
  waiting: "border-ink-muted text-ink-muted border-dashed",
  checking: "border-ink text-ink border-dashed",
  problem: "border-ink text-ink border-[3px]",
};

export function Stamp({
  label,
  description,
  variant,
}: {
  label: string;
  description: string;
  variant: StampVariant;
}) {
  return (
    <span
      role="img"
      aria-label={description}
      className={`stamp inline-block border px-2 py-1 text-sm font-semibold uppercase tracking-[0.12em] ${variantClass[variant]}`}
    >
      {label}
    </span>
  );
}
