import { apiBaseUrl } from "@/lib/api/client";

// Same-origin proxy for the API's Server-Sent Events stream, so the browser
// never needs the API's address and no CORS is involved.
export async function GET(
  request: Request,
  { params }: { params: Promise<{ code: string }> },
) {
  const { code } = await params;
  if (!/^WB(-[0-9A-HJKMNP-TV-Z]{4}){4}$/.test(code)) {
    return new Response("Not found", { status: 404 });
  }
  let upstream: Response;
  try {
    upstream = await fetch(new URL(`/v1/track/${code}/events`, apiBaseUrl()), {
      headers: {
        Accept: "text/event-stream",
        ...(request.headers.get("last-event-id")
          ? { "Last-Event-ID": request.headers.get("last-event-id")! }
          : {}),
      },
      signal: request.signal, // close upstream when the browser goes away
      cache: "no-store",
    });
  } catch {
    return new Response("Upstream unavailable", { status: 502 });
  }
  if (!upstream.ok || !upstream.body) {
    return new Response(null, { status: upstream.status === 404 ? 404 : 502 });
  }
  return new Response(upstream.body, {
    headers: {
      "Content-Type": "text/event-stream",
      "Cache-Control": "no-cache, no-transform",
      Connection: "keep-alive",
      "X-Accel-Buffering": "no",
    },
  });
}
