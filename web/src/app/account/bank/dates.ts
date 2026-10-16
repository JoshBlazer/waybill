// Dates are shown in Lagos time: the contractors are in Nigeria, and the
// server and browser must render the same text.
const lagos = new Intl.DateTimeFormat("en-GB", {
  timeZone: "Africa/Lagos",
  weekday: "short",
  day: "numeric",
  month: "short",
  hour: "2-digit",
  minute: "2-digit",
  hour12: false,
});

/** "on Sat 17 Oct at 14:29 (Lagos time)" */
export function formatLagosDate(iso: string): string {
  const parts = Object.fromEntries(
    lagos.formatToParts(new Date(iso)).map((p) => [p.type, p.value]),
  );
  return `on ${parts.weekday} ${parts.day} ${parts.month} at ${parts.hour}:${parts.minute} (Lagos time)`;
}
