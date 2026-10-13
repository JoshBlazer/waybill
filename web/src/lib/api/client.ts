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

export type PaymentLink = components["schemas"]["PaymentLink"];
export type Tracking = components["schemas"]["Tracking"];
export type Invoice = components["schemas"]["Invoice"];
export type Problem = components["schemas"]["Problem"];
export type CreateInvoiceRequest =
  components["schemas"]["CreateInvoiceRequest"];

/** A public lookup has three outcomes. "unreachable" means we could not find
 *  out, and is shown differently from "not found". */
export type Lookup<T> =
  | { kind: "ok"; value: T }
  | { kind: "not_found" }
  | { kind: "unreachable"; reason: string };

async function lookup<T>(
  baseUrl: string,
  path: string,
  init?: RequestInit,
): Promise<Lookup<T>> {
  let res: Response;
  try {
    res = await fetch(new URL(path, baseUrl), { cache: "no-store", ...init });
  } catch {
    return { kind: "unreachable", reason: "network error" };
  }
  if (res.status === 404) return { kind: "not_found" };
  if (res.status !== 200) {
    return { kind: "unreachable", reason: `HTTP ${res.status}` };
  }
  return { kind: "ok", value: (await res.json()) as T };
}

export function fetchPaymentLink(
  baseUrl: string,
  code: string,
  init?: RequestInit,
) {
  return lookup<PaymentLink>(
    baseUrl,
    `/v1/pay/${encodeURIComponent(code)}`,
    init,
  );
}

export function fetchTracking(
  baseUrl: string,
  code: string,
  init?: RequestInit,
) {
  return lookup<Tracking>(
    baseUrl,
    `/v1/track/${encodeURIComponent(code)}`,
    init,
  );
}

export type CreateResult =
  | { kind: "created"; invoice: Invoice }
  | { kind: "problem"; problem: Problem }
  | { kind: "unreachable"; reason: string };

/** Server-side only: needs a contractor API key. */
export async function createInvoice(
  baseUrl: string,
  apiKey: string,
  idempotencyKey: string,
  body: CreateInvoiceRequest,
): Promise<CreateResult> {
  let res: Response;
  try {
    res = await fetch(new URL("/v1/invoices", baseUrl), {
      method: "POST",
      cache: "no-store",
      headers: {
        Authorization: `Bearer ${apiKey}`,
        "Idempotency-Key": idempotencyKey,
        "Content-Type": "application/json",
      },
      body: JSON.stringify(body),
      signal: AbortSignal.timeout(10_000),
    });
  } catch {
    return { kind: "unreachable", reason: "network error" };
  }
  if (res.status === 201) {
    return { kind: "created", invoice: (await res.json()) as Invoice };
  }
  if (res.headers.get("content-type")?.includes("json")) {
    return { kind: "problem", problem: (await res.json()) as Problem };
  }
  return { kind: "unreachable", reason: `HTTP ${res.status}` };
}

/** Where the Next.js server reaches the API. */
export function apiBaseUrl(): string {
  return process.env.API_INTERNAL_URL ?? "http://localhost:8080";
}
