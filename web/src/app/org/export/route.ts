import { exportQuery, proxyDownload } from "@/lib/api/download";
import { currentMember } from "@/lib/member";

// The organisation's accountant export (CSV), as the member this server
// acts as.
export async function GET(request: Request) {
  const q = exportQuery(new URL(request.url));
  if (!q) {
    return new Response("Choose a start and an end day.", { status: 422 });
  }
  const { key } = await currentMember();
  return proxyDownload(`/v1/exports/ledger.csv${q}`, key);
}
