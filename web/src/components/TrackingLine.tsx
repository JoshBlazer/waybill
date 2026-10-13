import type { Tracking } from "@/lib/api/client";
import { Stamp } from "./Stamp";

type StepView = Tracking["steps"][number];

const titles: Record<StepView["step"], string> = {
  received: "Received",
  confirmed: "Confirmed",
  converted: "Converted",
  sent_to_bank: "Sent to bank",
};

// Plain words for every step and status (docs/DESIGN.md §1).
function sentence(step: StepView): string {
  const s = `${step.step}:${step.status}`;
  switch (s) {
    case "received:done":
      return "We received the payment.";
    case "received:current":
      return "Waiting for the payment to arrive.";
    case "received:checking":
      return "We saw this payment, but the network undid it. We're watching for it again.";
    case "confirmed:done":
      return "The payment is final and can't be undone.";
    case "confirmed:current":
      return "Waiting for the network to make the payment final. This usually takes a few seconds to a few minutes.";
    case "converted:current":
      return "Changing the payment to naira at the locked rate.";
    case "converted:done":
      return "Changed to naira at the locked rate.";
    case "converted:skipped":
      return "Not needed: the contractor keeps the stablecoin.";
    case "sent_to_bank:current":
      return "Sending to the contractor's bank.";
    case "sent_to_bank:done":
      return "Sent to the contractor's bank.";
    case "sent_to_bank:skipped":
      return "Not needed: the money stays in the contractor's Waybill balance.";
    default:
      return step.status === "checking"
        ? "We're checking. This is not a failure."
        : "Not started yet.";
  }
}

const timeFormat = new Intl.DateTimeFormat("en-NG", {
  hour: "2-digit",
  minute: "2-digit",
  day: "numeric",
  month: "short",
  timeZone: "Africa/Lagos",
});

function Dot({ status }: { status: StepView["status"] }) {
  const base = "mt-1 h-4 w-4 shrink-0 rounded-full border-2";
  const fill =
    status === "done"
      ? "border-ink bg-ink"
      : status === "current" || status === "checking"
        ? "border-ink bg-paper"
        : "border-ink-muted border-dashed bg-paper";
  return <span aria-hidden="true" className={`${base} ${fill}`} />;
}

/** The four-station tracking line. Pure: renders the same on the server and
 *  in the live client island. */
export function TrackingLine({ tracking }: { tracking: Tracking }) {
  return (
    <ol className="flex flex-col gap-5" aria-label="Payment progress">
      {tracking.steps.map((step) => (
        <li key={step.step} className="flex gap-3">
          <Dot status={step.status} />
          <div className="flex flex-col gap-1">
            <div className="flex flex-wrap items-center gap-2">
              <h3 className="font-semibold">{titles[step.step]}</h3>
              {step.status === "current" && (
                <Stamp
                  variant="waiting"
                  label="In progress"
                  description={`${titles[step.step]}: in progress`}
                />
              )}
              {step.status === "checking" && (
                <Stamp
                  variant="checking"
                  label="Checking"
                  description={`${titles[step.step]}: checking`}
                />
              )}
            </div>
            <p
              className={
                step.status === "waiting" || step.status === "skipped"
                  ? "text-ink-muted"
                  : ""
              }
            >
              {sentence(step)}
            </p>
            {step.at && (
              <p className="font-mono text-sm text-ink-muted">
                <time dateTime={step.at}>
                  {timeFormat.format(new Date(step.at))}
                </time>
              </p>
            )}
          </div>
        </li>
      ))}
    </ol>
  );
}

/** The headline stamp for the whole payment. */
export function OverallStamp({ tracking }: { tracking: Tracking }) {
  if (tracking.invoiceState === "settled") {
    const toBank = tracking.steps.find((s) => s.step === "sent_to_bank");
    return toBank?.status === "done" ? (
      <Stamp
        variant="delivered"
        label="Sent to bank"
        description="Delivered: sent to the contractor's bank."
      />
    ) : toBank?.status === "skipped" ? (
      <Stamp
        variant="delivered"
        label="Paid in full"
        description="Delivered: paid in full and final."
      />
    ) : (
      <Stamp
        variant="confirmed"
        label="Confirmed"
        description="The payment is final."
      />
    );
  }
  if (tracking.invoiceState === "expired") {
    return (
      <Stamp
        variant="problem"
        label="Expired"
        description="This payment link expired before a payment arrived."
      />
    );
  }
  if (tracking.invoiceState === "cancelled") {
    return (
      <Stamp
        variant="problem"
        label="Cancelled"
        description="The contractor cancelled this invoice."
      />
    );
  }
  if (tracking.invoiceState === "open") {
    return (
      <Stamp
        variant="waiting"
        label="Waiting"
        description="Waiting for the payment."
      />
    );
  }
  return (
    <Stamp
      variant="received"
      label="Received"
      description="The payment has been received."
    />
  );
}
