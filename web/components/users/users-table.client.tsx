"use client";

import { useMemo, useState } from "react";
import { Search } from "lucide-react";

import { Input } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { RelativeTime } from "@/components/shared/relative-time";
import { RoleSelect } from "@/components/users/role-select.client";
import type { AdminUser } from "@/types/api";

type Props = {
  users: AdminUser[];
  currentID: string;
  // Optional toolbar slot — typically the "New user" button so it
  // sits next to the search + filter controls instead of on the
  // page header. Matches the toolbar-on-the-right convention used
  // by groups, profiles, and service-accounts admin pages.
  action?: React.ReactNode;
};

// UsersTable is the admin list with a client-side filter across
// name + email. Users grow past "scroll is fine" quickly in
// bigger orgs — even 30 is past the scan-by-eye threshold. The
// list already lands once at page render (no pagination today),
// so filtering in the browser is instant + zero round-trips.
//
// Role filter is a secondary dropdown — common investigation
// flow is "who are my admins?" / "who hasn't logged in recently?".
export function UsersTable({ users, currentID, action }: Props) {
  const [query, setQuery] = useState("");
  const [roleFilter, setRoleFilter] = useState<string>("all");

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    return users.filter((u) => {
      if (roleFilter !== "all" && u.role !== roleFilter) return false;
      if (!q) return true;
      return (
        u.email.toLowerCase().includes(q) ||
        (u.name ?? "").toLowerCase().includes(q)
      );
    });
  }, [users, query, roleFilter]);

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative max-w-sm flex-1">
          <Search
            className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground"
            aria-hidden
          />
          <Input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder={`Filter by name or email (${users.length} total)`}
            className="pl-8"
          />
        </div>
        <select
          value={roleFilter}
          onChange={(e) => setRoleFilter(e.target.value)}
          className="h-9 rounded-md border bg-background px-2 text-sm"
          aria-label="Filter by role"
        >
          <option value="all">All roles</option>
          <option value="admin">admin</option>
          <option value="maintainer">maintainer</option>
          <option value="viewer">viewer</option>
        </select>
        <span className="ml-auto text-xs text-muted-foreground tabular-nums">
          {filtered.length} of {users.length}
        </span>
        {action ? <div className="shrink-0">{action}</div> : null}
      </div>

      {filtered.length === 0 ? (
        <div className="rounded-md border bg-muted/20 py-8 text-center text-sm text-muted-foreground">
          No users match the current filters.
        </div>
      ) : (
        <div className="overflow-hidden rounded-lg border border-border bg-card">
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
              {filtered.map((u) => {
                const self = u.id === currentID;
                return (
                  <TableRow key={u.id}>
                    <TableCell>
                      <div className="flex flex-col">
                        <span className="font-medium">
                          {u.name || u.email}
                        </span>
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
                      <RelativeTime
                        at={u.last_login_at ?? null}
                        fallback="never"
                      />
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  );
}
