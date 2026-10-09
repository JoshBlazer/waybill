// Typed access to the Waybill API. Types come from schema.d.ts, which is
// generated from openapi/waybill.yaml; a contract change breaks this file at
// compile time instead of at runtime.
import type { components, operations } from "./schema";

export type Health = components["schemas"]["Health"];

type HealthResponses = operations["getHealth"]["responses"];
type HealthBody = HealthResponses[200]["content"]["application/json"];

/** The three answers a health probe can give. "unreachable" is distinct from
 *  "degraded": we could not find out, which is not the same as a no. */
export type HealthResult =
  | { kind: "ok"; health: HealthBody }
  | { kind: "degraded"; health: HealthBody }
  | { kind: "unreachable"; reason: string };

export async function fetchHealth(
  baseUrl: string,
  init?: RequestInit,
): Promise<HealthResult> {
  let res: Response;
  try {
    res = await fetch(new URL("/v1/health", baseUrl), {
      cache: "no-store",
      ...init,
    });
  } catch {
    return { kind: "unreachable", reason: "network error" };
  }
  if (res.status !== 200 && res.status !== 503) {
    return { kind: "unreachable", reason: `HTTP ${res.status}` };
  }
  const health = (await res.json()) as HealthBody;
  return res.status === 200
    ? { kind: "ok", health }
    : { kind: "degraded", health };
}
