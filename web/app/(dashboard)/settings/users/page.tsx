import type { Metadata } from "next";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Toaster } from "@/components/ui/sonner";
import { RelativeTime } from "@/components/shared/relative-time";
import { RoleSelect } from "@/components/users/role-select.client";
import { listAdminUsers } from "@/server/queries/admin";
import { resolveAuthState } from "@/server/queries/auth";

export const metadata: Metadata = {
  title: "Settings — Users",
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
      <Toaster position="top-right" richColors />
      <header className="space-y-1">
        <p className="text-sm text-muted-foreground">
          Every user known to this control plane. Roles follow a
          hierarchy: admin ≥ maintainer ≥ viewer. Viewers can read the
          dashboard; maintainers trigger runs and edit secrets; admins
          also manage users, integrations, and retention policy.
          Self-demotion is refused — promote another user to admin
          first if you need to step down.
        </p>
      </header>

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>User</TableHead>
            <TableHead>Provider</TableHead>
            <TableHead>Role</TableHead>
            <TableHead>Last login</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {data.users.map((u) => {
            const self = u.id === currentID;
            return (
              <TableRow key={u.id}>
                <TableCell>
                  <div className="flex flex-col">
                    <span className="font-medium">{u.name || u.email}</span>
                    <span className="text-xs text-muted-foreground">
                      {u.email}
                    </span>
                  </div>
                </TableCell>
                <TableCell className="text-xs text-muted-foreground">
                  {u.provider}
                </TableCell>
                <TableCell>
                  <RoleSelect
                    userID={u.id}
                    email={u.email}
                    currentRole={u.role}
                    self={self}
                  />
                  {self ? (
                    <span className="ml-2 text-[10px] uppercase tracking-wide text-muted-foreground">
                      you
                    </span>
                  ) : null}
                </TableCell>
                <TableCell className="text-muted-foreground">
                  <RelativeTime at={u.last_login_at ?? null} fallback="never" />
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </section>
  );
}
