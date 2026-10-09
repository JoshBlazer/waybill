import type { HealthResult } from "@/lib/api/client";
import { Stamp } from "./Stamp";

/** Shows API health in plain words, using the stamp motif. */
export function ServiceStatus({ result }: { result: HealthResult }) {
  if (result.kind === "unreachable") {
    return (
      <div className="flex flex-col items-start gap-2">
        <Stamp
          variant="checking"
          label="Checking"
          description="We couldn't reach the Waybill service."
        />
        <p className="text-ink-muted">
          We couldn&apos;t reach the Waybill service. This is not the same as it
          being down; try again in a moment.
        </p>
      </div>
    );
  }

  const { health } = result;
  const ok = result.kind === "ok";
  return (
    <div className="flex flex-col items-start gap-2">
      {ok ? (
        <Stamp
          variant="confirmed"
          label="Running"
          description="The Waybill service is running."
        />
      ) : (
        <Stamp
          variant="problem"
          label="Problem"
          description="The Waybill service is running, but its database is unavailable."
        />
      )}
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1">
        <dt className="text-ink-muted">Database</dt>
        <dd>{health.database === "ok" ? "Answering" : "Not answering"}</dd>
        <dt className="text-ink-muted">Networks</dt>
        <dd className="font-mono">
          {health.networks.length > 0 ? health.networks.join(", ") : "None"}
        </dd>
        <dt className="text-ink-muted">Money</dt>
        <dd>{health.testMode ? "Test money only" : "Unknown"}</dd>
        <dt className="text-ink-muted">Version</dt>
        <dd className="font-mono">{health.version}</dd>
      </dl>
    </div>
  );
}
