import type { Metadata } from "next";

import { GroupsManager } from "@/components/groups/groups-manager.client";
import {
  listAdminGroups,
  listAdminGroupMembers,
  listAdminUsers,
} from "@/server/queries/admin";

export const metadata: Metadata = {
  title: "Groups — gocdnext",
};

// force-dynamic so group mutations from another tab (or even
// this one, post-Server-Action) reflect immediately after the
// revalidatePath. The payload is small — 1 list + N member
// lists where N is "tens at most" per realistic deploy.
export const dynamic = "force-dynamic";

export default async function GroupsPage() {
  const [{ groups }, { users }] = await Promise.all([
    listAdminGroups(),
    listAdminUsers(),
  ]);

  // Eager-load members per group so expansion is instant. N is
  // "tens", member-list-per-group rarely exceeds a dozen; total
  // payload stays well under 50 KB even in a busy org.
  const membersByGroup: Record<string, Awaited<
    ReturnType<typeof listAdminGroupMembers>
  >["members"]> = {};
  await Promise.all(
    groups.map(async (g) => {
      const { members } = await listAdminGroupMembers(g.id);
      membersByGroup[g.id] = members;
    }),
  );

  return (
    <section className="space-y-6">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">Groups</h2>
        <p className="text-sm text-muted-foreground">
          Collective approvers — reference by name in{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">
            approver_groups:
          </code>{" "}
          on any pipeline&apos;s approval gate.
        </p>
      </div>

      <GroupsManager
        initial={groups}
        users={users}
        membersByGroup={membersByGroup}
      />
    </section>
  );
}
