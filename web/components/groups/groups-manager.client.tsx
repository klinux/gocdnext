"use client";

import { useMemo, useState, useTransition } from "react";
import {
  Loader2,
  Pencil,
  Plus,
  Search,
  Trash2,
  UserPlus,
  UsersRound,
  X,
} from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  createGroup,
  updateGroup,
  deleteGroup,
  addGroupMember,
  removeGroupMember,
} from "@/server/actions/groups";
import type { AdminGroup, AdminGroupMember } from "@/server/queries/admin";
import type { AdminUser } from "@/types/api";

type Props = {
  initial: AdminGroup[];
  users: AdminUser[];
  membersByGroup: Record<string, AdminGroupMember[]>;
};

type FormDraft = {
  id: string | null;
  name: string;
  description: string;
};

function blankForm(): FormDraft {
  return { id: null, name: "", description: "" };
}

export function GroupsManager({ initial, users, membersByGroup }: Props) {
  const [groups, setGroups] = useState<AdminGroup[]>(initial);
  const [members, setMembers] = useState<Record<string, AdminGroupMember[]>>(
    membersByGroup,
  );
  const [filter, setFilter] = useState("");
  const [form, setForm] = useState<FormDraft | null>(null);
  const [membersSheet, setMembersSheet] = useState<AdminGroup | null>(null);
  const [addingUser, setAddingUser] = useState("");
  const [pending, startTransition] = useTransition();

  const filtered = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q) return groups;
    return groups.filter(
      (g) =>
        g.name.toLowerCase().includes(q) ||
        g.description.toLowerCase().includes(q),
    );
  }, [groups, filter]);

  const saveForm = () => {
    if (!form) return;
    const name = form.name.trim();
    if (!name) {
      toast.error("Name is required");
      return;
    }
    startTransition(async () => {
      const body = {
        name,
        description: form.description,
      };
      const res = form.id
        ? await updateGroup({ ...body, id: form.id })
        : await createGroup(body);
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      setGroups((prev) => {
        if (form.id) {
          return prev.map((g) =>
            g.id === form.id
              ? { ...g, name, description: form.description }
              : g,
          );
        }
        return [
          ...prev,
          {
            id: "__opt__" + Date.now(),
            name,
            description: form.description,
            member_count: 0,
            created_at: new Date().toISOString(),
            updated_at: new Date().toISOString(),
          },
        ].sort((a, b) => a.name.localeCompare(b.name));
      });
      toast.success(form.id ? "Group updated" : "Group created");
      setForm(null);
    });
  };

  const removeGroup = (g: AdminGroup) => {
    if (
      !confirm(
        `Delete group "${g.name}"? Approval gates referencing this group by name will silently skip it.`,
      )
    ) {
      return;
    }
    startTransition(async () => {
      const res = await deleteGroup({ id: g.id });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      setGroups((prev) => prev.filter((x) => x.id !== g.id));
      toast.success("Group deleted");
    });
  };

  const addMember = () => {
    if (!membersSheet || !addingUser) return;
    const groupID = membersSheet.id;
    const userID = addingUser;
    startTransition(async () => {
      const res = await addGroupMember({ groupID, userID });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      const picked = users.find((u) => u.id === userID);
      if (picked) {
        setMembers((prev) => {
          const prior = prev[groupID] ?? [];
          if (prior.some((m) => m.user_id === userID)) return prev;
          const next = [
            ...prior,
            {
              user_id: userID,
              email: picked.email,
              name: picked.name ?? "",
              role: picked.role,
              added_at: new Date().toISOString(),
            },
          ].sort((a, b) => (a.name || a.email).localeCompare(b.name || b.email));
          return { ...prev, [groupID]: next };
        });
        setGroups((prev) =>
          prev.map((g) =>
            g.id === groupID ? { ...g, member_count: g.member_count + 1 } : g,
          ),
        );
      }
      setAddingUser("");
      toast.success("Member added");
    });
  };

  const removeMember = (groupID: string, userID: string) => {
    startTransition(async () => {
      const res = await removeGroupMember({ groupID, userID });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      setMembers((prev) => ({
        ...prev,
        [groupID]: (prev[groupID] ?? []).filter((m) => m.user_id !== userID),
      }));
      setGroups((prev) =>
        prev.map((g) =>
          g.id === groupID
            ? { ...g, member_count: Math.max(0, g.member_count - 1) }
            : g,
        ),
      );
      toast.success("Member removed");
    });
  };

  const activeMembers = membersSheet ? members[membersSheet.id] ?? [] : [];
  const usedIDs = new Set(activeMembers.map((m) => m.user_id));
  const availableUsers = users.filter((u) => !usedIDs.has(u.id));

  return (
    <div className="space-y-4">
      {/* Toolbar: filter + add */}
      <div className="flex items-center justify-between gap-4">
        <div className="relative max-w-sm flex-1">
          <Search
            className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground"
            aria-hidden
          />
          <Input
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="Filter by name or description"
            className="pl-8"
          />
        </div>
        <Button onClick={() => setForm(blankForm())} disabled={pending}>
          <Plus className="mr-2 size-4" aria-hidden />
          New group
        </Button>
      </div>

      {/* Table of groups */}
      {filtered.length === 0 ? (
        <div className="rounded-md border bg-muted/20 py-12 text-center">
          <UsersRound
            className="mx-auto size-8 text-muted-foreground/60"
            aria-hidden
          />
          <p className="mt-3 text-sm font-medium">
            {groups.length === 0
              ? "No groups yet"
              : `No groups match "${filter}"`}
          </p>
          <p className="mt-1 text-xs text-muted-foreground">
            {groups.length === 0
              ? "Add one to let a team approve gates collectively."
              : "Adjust the filter or clear it to see every group."}
          </p>
        </div>
      ) : (
        <div className="rounded-md border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Description</TableHead>
                <TableHead className="w-28 text-right">Members</TableHead>
                <TableHead className="w-40 text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {filtered.map((g) => (
                <TableRow key={g.id} className="hover:bg-muted/40">
                  <TableCell className="font-medium">
                    <button
                      type="button"
                      onClick={() => setMembersSheet(g)}
                      className="text-left hover:underline"
                    >
                      {g.name}
                    </button>
                  </TableCell>
                  <TableCell className="text-muted-foreground">
                    {g.description || (
                      <span className="text-xs italic">—</span>
                    )}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    <button
                      type="button"
                      onClick={() => setMembersSheet(g)}
                      className="inline-flex items-center gap-1 rounded-md bg-muted/60 px-2 py-0.5 text-xs hover:bg-muted"
                    >
                      <UsersRound className="size-3" />
                      {g.member_count}
                    </button>
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="inline-flex items-center gap-1">
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => setMembersSheet(g)}
                        aria-label={`Manage members of ${g.name}`}
                      >
                        <UserPlus className="size-4" />
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() =>
                          setForm({
                            id: g.id,
                            name: g.name,
                            description: g.description,
                          })
                        }
                        aria-label={`Edit ${g.name}`}
                      >
                        <Pencil className="size-4" />
                      </Button>
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => removeGroup(g)}
                        aria-label={`Delete ${g.name}`}
                      >
                        <Trash2 className="size-4" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      {/* Edit/Create sheet */}
      <Sheet
        open={form !== null}
        onOpenChange={(open) => !open && setForm(null)}
      >
        <SheetContent side="right" className="data-[side=right]:sm:max-w-md">
          <SheetHeader>
            <SheetTitle>
              {form?.id ? "Edit group" : "New group"}
            </SheetTitle>
            <SheetDescription>
              Group names reference YAML{" "}
              <code className="rounded bg-muted px-1 text-xs">approver_groups:</code>{" "}
              — changing the name propagates cleanly since gates store names.
            </SheetDescription>
          </SheetHeader>
          <div className="space-y-4 px-6 pb-6">
            <div className="space-y-2">
              <Label htmlFor="group-name">Name</Label>
              <Input
                id="group-name"
                value={form?.name ?? ""}
                onChange={(e) =>
                  setForm((f) => (f ? { ...f, name: e.target.value } : f))
                }
                placeholder="sre"
                disabled={pending}
              />
              <p className="text-xs text-muted-foreground">
                Letters, digits, dash, underscore, dot only.
              </p>
            </div>
            <div className="space-y-2">
              <Label htmlFor="group-desc">Description</Label>
              <Input
                id="group-desc"
                value={form?.description ?? ""}
                onChange={(e) =>
                  setForm((f) =>
                    f ? { ...f, description: e.target.value } : f,
                  )
                }
                placeholder="On-call SRE rotation"
                disabled={pending}
              />
            </div>
            <div className="flex items-center gap-2">
              <Button onClick={saveForm} disabled={pending}>
                {pending ? (
                  <Loader2 className="mr-2 size-4 animate-spin" />
                ) : null}
                {form?.id ? "Save changes" : "Create group"}
              </Button>
              <Button
                variant="ghost"
                onClick={() => setForm(null)}
                disabled={pending}
              >
                Cancel
              </Button>
            </div>
          </div>
        </SheetContent>
      </Sheet>

      {/* Members sheet — full-height drawer */}
      <Sheet
        open={membersSheet !== null}
        onOpenChange={(open) => {
          if (!open) {
            setMembersSheet(null);
            setAddingUser("");
          }
        }}
      >
        <SheetContent
          side="right"
          className="flex flex-col gap-0 p-0 data-[side=right]:w-[28rem] data-[side=right]:sm:max-w-[28rem]"
        >
          <SheetHeader className="border-b p-6">
            <SheetTitle>
              {membersSheet?.name ?? ""}
            </SheetTitle>
            <SheetDescription>
              {membersSheet?.description || "No description"}
            </SheetDescription>
          </SheetHeader>
          <div className="flex-1 overflow-y-auto px-6 py-4">
            {activeMembers.length === 0 ? (
              <div className="rounded-md border bg-muted/20 py-8 text-center text-sm text-muted-foreground">
                No members yet.
              </div>
            ) : (
              <ul className="divide-y rounded-md border">
                {activeMembers.map((m) => (
                  <li
                    key={m.user_id}
                    className="flex items-center justify-between gap-2 px-3 py-2"
                  >
                    <div className="min-w-0">
                      <p className="truncate text-sm font-medium">
                        {m.name || m.email}
                      </p>
                      <p className="truncate text-xs text-muted-foreground">
                        {m.email} · {m.role}
                      </p>
                    </div>
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() =>
                        membersSheet && removeMember(membersSheet.id, m.user_id)
                      }
                      disabled={pending}
                      aria-label={`Remove ${m.email}`}
                    >
                      <X className="size-4" />
                    </Button>
                  </li>
                ))}
              </ul>
            )}
          </div>
          <div className="space-y-2 border-t bg-muted/20 p-4">
            <Label htmlFor="add-member" className="text-xs">
              Add member
            </Label>
            <div className="flex items-center gap-2">
              <select
                id="add-member"
                className="flex h-9 flex-1 rounded-md border bg-background px-2 text-sm"
                value={addingUser}
                onChange={(e) => setAddingUser(e.target.value)}
                disabled={pending || availableUsers.length === 0}
              >
                <option value="">
                  {availableUsers.length === 0
                    ? "All users already in this group"
                    : "Pick a user…"}
                </option>
                {availableUsers.map((u) => (
                  <option key={u.id} value={u.id}>
                    {u.name || u.email} ({u.email})
                  </option>
                ))}
              </select>
              <Button
                onClick={addMember}
                disabled={pending || !addingUser}
                size="sm"
              >
                <UserPlus className="mr-1.5 size-3.5" />
                Add
              </Button>
            </div>
          </div>
        </SheetContent>
      </Sheet>
    </div>
  );
}
