import { proxyDownload, UUID } from "@/lib/api/download";
import { currentMember } from "@/lib/member";

// The run's receipt as a PDF, fetched as the member this server acts as.
export async function GET(
  _request: Request,
  { params }: { params: Promise<{ id: string }> },
) {
  const { id } = await params;
  if (!UUID.test(id)) {
    return new Response("There is no such document.", { status: 404 });
  }
  const { key } = await currentMember();
  return proxyDownload(`/v1/batches/${id}/receipt.pdf`, key);
}
