// The demo organisation has two members. The Next.js server acts as one of
// them, chosen with a cookie, using that member's API key from the server's
// environment. The browser never sees a key. Import only from server code.
import { cookies } from "next/headers";

export const MEMBER_COOKIE = "waybill-member";

export type DemoMember = "admin" | "approver";

export const demoMembers: Record<
  DemoMember,
  { name: string; roles: string; env: string }
> = {
  admin: {
    name: "Tolu Bakare",
    roles: "admin, builds runs",
    env: "WAYBILL_DEV_ADMIN_KEY",
  },
  approver: {
    name: "Chidi Eze",
    roles: "approver",
    env: "WAYBILL_DEV_APPROVER_KEY",
  },
};

/** The member the server acts as, and their key (undefined when this server
 *  has no demo organisation). */
export async function currentMember(): Promise<{
  who: DemoMember;
  key: string | undefined;
}> {
  const jar = await cookies();
  const who: DemoMember =
    jar.get(MEMBER_COOKIE)?.value === "approver" ? "approver" : "admin";
  return { who, key: process.env[demoMembers[who].env] };
}
