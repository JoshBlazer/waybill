import type { Batch } from "@/lib/api/org";

/** Plain words for a run's state. */
export const stateLabel: Record<Batch["state"], string> = {
  draft: "Needs fixing",
  ready: "Ready to send",
  awaiting_approval: "Waiting for approval",
  approved: "Approved, ready to send",
  sending: "Sending",
  confirming: "Sent, confirming",
  paid: "Paid",
  failed: "Failed, nothing was paid",
  rejected: "Rejected",
  cancelled: "Cancelled",
};

/** How many line rows the new-run form offers. */
export const LINE_ROWS = 3;
