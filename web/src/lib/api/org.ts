// Server-side helpers for the stage 4 organisation and batch-run API. Every
// call needs a member's (or, for wallets, the contractor's) API key, which
// only the Next.js server holds.
import type { components } from "./schema";
import type { Lookup, Problem } from "./client";

export type Org = components["schemas"]["Org"];
export type Member = components["schemas"]["Member"];
export type Funding = components["schemas"]["Funding"];
export type Batch = components["schemas"]["Batch"];
export type BatchLine = components["schemas"]["BatchLine"];
export type PayoutWallet = components["schemas"]["PayoutWallet"];

export type Result<T> =
  | { kind: "ok"; value: T }
  | { kind: "problem"; problem: Problem }
  | { kind: "unreachable"; reason: string };

async function request<T>(
  baseUrl: string,
  key: string,
  method: string,
  path: string,
  body?: unknown,
  headers: Record<string, string> = {},
): Promise<Result<T>> {
  let res: Response;
  try {
    res = await fetch(new URL(path, baseUrl), {
      method,
      cache: "no-store",
      headers: {
        Authorization: `Bearer ${key}`,
        ...(body === undefined ? {} : { "Content-Type": "application/json" }),
        ...headers,
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(15_000),
    });
  } catch {
    return { kind: "unreachable", reason: "network error" };
  }
  if (res.ok) return { kind: "ok", value: (await res.json()) as T };
  if (res.headers.get("content-type")?.includes("json")) {
    return { kind: "problem", problem: (await res.json()) as Problem };
  }
  return { kind: "unreachable", reason: `HTTP ${res.status}` };
}

/** Collapses a Result into a Lookup for pages that only read. */
export function asLookup<T>(r: Result<T>): Lookup<T> {
  if (r.kind === "ok") return r;
  if (r.kind === "problem" && r.problem.status === 404)
    return { kind: "not_found" };
  return {
    kind: "unreachable",
    reason: r.kind === "problem" ? r.problem.title : r.reason,
  };
}

export const getOrg = (b: string, k: string) =>
  request<Org>(b, k, "GET", "/v1/org");
export const listMembers = (b: string, k: string) =>
  request<{ data: Member[] }>(b, k, "GET", "/v1/org/members");
export const getFunding = (b: string, k: string) =>
  request<Funding>(b, k, "GET", "/v1/org/funding");
export const listBatches = (b: string, k: string) =>
  request<{ data: Batch[] }>(b, k, "GET", "/v1/batches");
export const getBatch = (b: string, k: string, id: string) =>
  request<Batch>(b, k, "GET", `/v1/batches/${encodeURIComponent(id)}`);

export type NewLine = {
  contractorId: string;
  amount: string;
  reference: string;
};

/** Builds and checks a run. Safe to retry with the same idempotency key. */
export const createBatch = (
  b: string,
  k: string,
  idempotencyKey: string,
  lines: NewLine[],
) =>
  request<Batch>(
    b,
    k,
    "POST",
    "/v1/batches",
    { lines },
    {
      "Idempotency-Key": idempotencyKey,
    },
  );

export type BatchAction = "approve" | "reject" | "send" | "cancel";

export const batchAction = (
  b: string,
  k: string,
  id: string,
  action: BatchAction,
) =>
  request<Batch>(
    b,
    k,
    "POST",
    `/v1/batches/${encodeURIComponent(id)}/${action}`,
  );

export const listPayoutWallets = (b: string, k: string) =>
  request<{ data: PayoutWallet[] }>(b, k, "GET", "/v1/me/payout-wallet");

export const setPayoutWallet = (
  b: string,
  k: string,
  network: string,
  address: string,
) =>
  request<PayoutWallet>(b, k, "PUT", "/v1/me/payout-wallet", {
    network,
    address,
  });

// Contractor balance and conversion (stage 4, ADR-044).
export type Balance = components["schemas"]["Balance"];
export type Conversion = components["schemas"]["Conversion"];

export const getBalance = (b: string, k: string) =>
  request<Balance>(b, k, "GET", "/v1/me/balance");

export const getConversion = (b: string, k: string, id: string) =>
  request<Conversion>(
    b,
    k,
    "GET",
    `/v1/me/conversions/${encodeURIComponent(id)}`,
  );

export const quoteConversion = (b: string, k: string, amount: string) =>
  request<Conversion>(b, k, "POST", "/v1/me/conversions", { amount });

/** Accepting is safe to repeat: the key is fixed per conversion. */
export const acceptConversion = (b: string, k: string, id: string) =>
  request<Conversion>(
    b,
    k,
    "POST",
    `/v1/me/conversions/${encodeURIComponent(id)}/accept`,
    undefined,
    { "Idempotency-Key": `accept-${id}` },
  );
