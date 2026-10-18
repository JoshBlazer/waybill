import { apiBaseUrl } from "./client";

// Same-origin downloads of API documents (receipts, PDFs, CSV exports).
// The server adds the demo key, so the browser never holds one; the body
// streams straight through.

const messages: Record<number, string> = {
  401: "This server is not set up for documents.",
  404: "There is no such document.",
  409: "There is no receipt yet: the payment is not complete.",
  422: "Choose a range of 1 to 366 days: the start day is included, the end day is not.",
};

export async function proxyDownload(
  path: string,
  key: string | undefined,
): Promise<Response> {
  if (!key) {
    return text(401);
  }
  let upstream: Response;
  try {
    upstream = await fetch(new URL(path, apiBaseUrl()), {
      headers: { Authorization: `Bearer ${key}` },
      cache: "no-store",
    });
  } catch {
    return new Response("We can't reach the server right now.", {
      status: 502,
    });
  }
  if (!upstream.ok || !upstream.body) {
    return text(upstream.status in messages ? upstream.status : 502);
  }
  const headers = new Headers({
    "Content-Type":
      upstream.headers.get("Content-Type") ?? "application/octet-stream",
    "Cache-Control": "no-store",
  });
  const disposition = upstream.headers.get("Content-Disposition");
  if (disposition) {
    headers.set("Content-Disposition", disposition);
  }
  return new Response(upstream.body, { headers });
}

function text(status: number): Response {
  return new Response(
    messages[status] ?? "Something went wrong. Try again in a minute.",
    { status, headers: { "Content-Type": "text/plain; charset=utf-8" } },
  );
}

const DATE = /^\d{4}-\d{2}-\d{2}$/;

/** The export query, or null when a date is missing or malformed. */
export function exportQuery(url: URL): string | null {
  const from = url.searchParams.get("from") ?? "";
  const to = url.searchParams.get("to") ?? "";
  if (!DATE.test(from) || !DATE.test(to)) {
    return null;
  }
  return `?from=${from}&to=${to}`;
}

export const UUID =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
