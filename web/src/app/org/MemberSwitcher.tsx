import { demoMembers, type DemoMember } from "@/lib/member";
import { switchMemberAction } from "./actions";

// Demo only: lets one person show dual approval by acting as either member.
export function MemberSwitcher({
  current,
  back,
}: {
  current: DemoMember;
  back: string;
}) {
  const other: DemoMember = current === "admin" ? "approver" : "admin";
  return (
    <form
      action={switchMemberAction}
      className="flex flex-wrap items-center gap-3 border border-dashed border-ink-muted p-3"
    >
      <p className="text-sm">
        Acting as{" "}
        <strong data-testid="acting-as">{demoMembers[current].name}</strong> (
        {demoMembers[current].roles}).
      </p>
      <input type="hidden" name="member" value={other} />
      <input type="hidden" name="back" value={back} />
      <button
        type="submit"
        className="min-h-11 border border-ink px-3 text-sm font-semibold"
      >
        Act as {demoMembers[other].name}
      </button>
    </form>
  );
}
