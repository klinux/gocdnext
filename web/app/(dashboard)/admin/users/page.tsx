import type { Metadata } from "next";
import { Users } from "lucide-react";

import { NewUserDialog } from "@/components/users/new-user-dialog.client";
import { UsersTable } from "@/components/users/users-table.client";
import { listAdminUsers } from "@/server/queries/admin";
import { resolveAuthState } from "@/server/queries/auth";

export const metadata: Metadata = {
  title: "Users — gocdnext",
};

// Forcing dynamic so a role change from another tab shows up on
// refresh without RSC cache staleness — the list is tiny so the
// per-nav no-store fetch is cheap.
export const dynamic = "force-dynamic";

export default async function UsersPage() {
  const [auth, data] = await Promise.all([
    resolveAuthState(),
    listAdminUsers(),
  ]);
  const currentID =
    auth.mode === "authenticated" ? auth.user.id : "";

  return (
    <section className="space-y-6">
      <header className="space-y-1">
        <h1 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
          <Users className="h-6 w-6" aria-hidden />
          Users
        </h1>
        <p className="text-sm text-muted-foreground">
          Every user known to this control plane. Roles follow a
          hierarchy: admin ≥ maintainer ≥ viewer. Viewers can read the
          dashboard; maintainers trigger runs and edit secrets; admins
          also manage users, integrations, and retention policy.
          Self-demotion is refused — promote another user to admin
          first if you need to step down.
        </p>
      </header>

      <UsersTable
        users={data.users}
        currentID={currentID}
        action={<NewUserDialog />}
      />
    </section>
  );
}
