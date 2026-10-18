import { exportQuery, proxyDownload } from "@/lib/api/download";

// The demo contractor's accountant export (CSV).
export async function GET(request: Request) {
  const q = exportQuery(new URL(request.url));
  if (!q) {
    return new Response("Choose a start and an end day.", { status: 422 });
  }
  return proxyDownload(
    `/v1/exports/ledger.csv${q}`,
    process.env.WAYBILL_DEV_CONTRACTOR_KEY,
  );
}
