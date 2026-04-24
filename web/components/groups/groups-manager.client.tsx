"use client";

import { useState, useTransition } from "react";
import { Loader2, Pencil, Plus, Save, Trash2, UserPlus, X } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
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
  // membersByGroup is a per-group map loaded upfront so a card
  // expansion doesn't round-trip to the server for tiny groups.
  // Empty object when the page wants to lazy-load per expansion.
  membersByGroup: Record<string, AdminGroupMember[]>;
};

type DraftForm = {
  id: string | null;
  name: string;
  description: string;
};

function blankDraft(): DraftForm {
  return { id: null, name: "", description: "" };
}

export function GroupsManager({ initial, users, membersByGroup }: Props) {
  const [groups, setGroups] = useState<AdminGroup[]>(initial);
  const [members, setMembers] = useState<Record<string, AdminGroupMember[]>>(
    membersByGroup,
  );
  const [expanded, setExpanded] = useState<string | null>(null);
  const [draft, setDraft] = useState<DraftForm | null>(null);
  const [memberDraftFor, setMemberDraftFor] = useState<string | null>(null);
  const [memberDraftUser, setMemberDraftUser] = useState<string>("");
  const [pending, startTransition] = useTransition();

  const save = () => {
    if (!draft) return;
    const name = draft.name.trim();
    if (!name) {
      toast.error("Name is required");
      return;
    }
    startTransition(async () => {
      const res = draft.id
        ? await updateGroup({
            id: draft.id,
            name,
            description: draft.description,
          })
        : await createGroup({ name, description: draft.description });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      toast.success(draft.id ? "Group updated" : "Group created");
      setGroups((prev) => {
        if (draft.id) {
          return prev.map((g) =>
            g.id === draft.id ? { ...g, name, description: draft.description } : g,
          );
        }
        return [
          ...prev,
          {
            id: "__optimistic__" + Date.now(),
            name,
            description: draft.description,
            member_count: 0,
            created_at: new Date().toISOString(),
            updated_at: new Date().toISOString(),
          },
        ].sort((a, b) => a.name.localeCompare(b.name));
      });
      setDraft(null);
    });
  };

  const remove = (g: AdminGroup) => {
    if (!confirm(`Delete group "${g.name}"? Approval gates referencing this group by name will silently skip it.`)) return;
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

  const addMember = (groupID: string) => {
    if (!memberDraftUser) return;
    startTransition(async () => {
      const res = await addGroupMember({ groupID, userID: memberDraftUser });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      const picked = users.find((u) => u.id === memberDraftUser);
      if (picked) {
        setMembers((prev) => {
          const prior = prev[groupID] ?? [];
          if (prior.some((m) => m.user_id === picked.id)) return prev;
          const next = [
            ...prior,
            {
              user_id: picked.id,
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
      setMemberDraftFor(null);
      setMemberDraftUser("");
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

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h3 className="text-lg font-semibold">Approver groups</h3>
          <p className="text-sm text-muted-foreground">
            Groups referenced by name in{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-xs">approver_groups:</code>
            {" "}on pipeline approval gates. Any member of a listed group can
            approve.
          </p>
        </div>
        {!draft ? (
          <Button onClick={() => setDraft(blankDraft())} disabled={pending}>
            <Plus className="mr-2 size-4" aria-hidden />
            New group
          </Button>
        ) : null}
      </div>

      {draft ? (
        <Card>
          <CardContent className="space-y-3 py-4">
            <div className="flex items-center justify-between">
              <h4 className="text-sm font-medium">
                {draft.id ? "Edit group" : "New group"}
              </h4>
              <Button
                variant="ghost"
                size="sm"
                onClick={() => setDraft(null)}
                disabled={pending}
              >
                <X className="size-4" aria-hidden />
              </Button>
            </div>
            <div className="space-y-2">
              <Label htmlFor="group-name">Name</Label>
              <Input
                id="group-name"
                value={draft.name}
                onChange={(e) =>
                  setDraft((d) => (d ? { ...d, name: e.target.value } : d))
                }
                placeholder="sre"
                disabled={pending}
              />
              <p className="text-xs text-muted-foreground">
                Used as-is in{" "}
                <code className="rounded bg-muted px-1 py-0.5">
                  approver_groups: [{draft.name || "sre"}]
                </code>
                . Letters, digits, dash, underscore, dot.
              </p>
            </div>
            <div className="space-y-2">
              <Label htmlFor="group-desc">Description (optional)</Label>
              <Input
                id="group-desc"
                value={draft.description}
                onChange={(e) =>
                  setDraft((d) =>
                    d ? { ...d, description: e.target.value } : d,
                  )
                }
                placeholder="On-call SRE rotation"
                disabled={pending}
              />
            </div>
            <div className="flex items-center gap-2">
              <Button onClick={save} disabled={pending}>
                {pending ? (
                  <Loader2 className="mr-2 size-4 animate-spin" />
                ) : (
                  <Save className="mr-2 size-4" />
                )}
                Save
              </Button>
              <Button
                variant="ghost"
                onClick={() => setDraft(null)}
                disabled={pending}
              >
                Cancel
              </Button>
            </div>
          </CardContent>
        </Card>
      ) : null}

      {groups.length === 0 && !draft ? (
        <Card>
          <CardContent className="py-8 text-center text-sm text-muted-foreground">
            No groups yet. Add one to let a team approve gates collectively.
          </CardContent>
        </Card>
      ) : null}

      <div className="space-y-2">
        {groups.map((g) => {
          const isOpen = expanded === g.id;
          const m = members[g.id] ?? [];
          const usedIDs = new Set(m.map((x) => x.user_id));
          const available = users.filter((u) => !usedIDs.has(u.id));
          return (
            <Card key={g.id}>
              <CardContent className="space-y-3 py-4">
                <div className="flex items-center justify-between gap-4">
                  <button
                    type="button"
                    className="flex-1 text-left"
                    onClick={() => setExpanded(isOpen ? null : g.id)}
                  >
                    <div className="font-medium">{g.name}</div>
                    {g.description ? (
                      <div className="mt-0.5 text-xs text-muted-foreground">
                        {g.description}
                      </div>
                    ) : null}
                    <div className="mt-1 text-xs text-muted-foreground">
                      {g.member_count}{" "}
                      {g.member_count === 1 ? "member" : "members"}
                    </div>
                  </button>
                  <div className="flex items-center gap-1">
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() =>
                        setDraft({
                          id: g.id,
                          name: g.name,
                          description: g.description,
                        })
                      }
                      disabled={pending}
                      aria-label={`Edit ${g.name}`}
                    >
                      <Pencil className="size-4" />
                    </Button>
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() => remove(g)}
                      disabled={pending}
                      aria-label={`Delete ${g.name}`}
                    >
                      <Trash2 className="size-4" />
                    </Button>
                  </div>
                </div>

                {isOpen ? (
                  <div className="space-y-2 rounded-md border p-3">
                    {m.length === 0 ? (
                      <p className="text-xs text-muted-foreground">
                        No members yet.
                      </p>
                    ) : (
                      m.map((member) => (
                        <div
                          key={member.user_id}
                          className="flex items-center justify-between text-sm"
                        >
                          <div>
                            <span className="font-medium">
                              {member.name || member.email}
                            </span>
                            <span className="ml-2 text-xs text-muted-foreground">
                              {member.email} · {member.role}
                            </span>
                          </div>
                          <Button
                            variant="ghost"
                            size="sm"
                            onClick={() => removeMember(g.id, member.user_id)}
                            disabled={pending}
                            aria-label={`Remove ${member.email}`}
                          >
                            <X className="size-4" />
                          </Button>
                        </div>
                      ))
                    )}

                    {memberDraftFor === g.id ? (
                      <div className="flex items-center gap-2 pt-2">
                        <select
                          className="flex h-9 flex-1 rounded-md border bg-transparent px-2 text-sm"
                          value={memberDraftUser}
                          onChange={(e) => setMemberDraftUser(e.target.value)}
                          disabled={pending}
                        >
                          <option value="">Select user…</option>
                          {available.map((u) => (
                            <option key={u.id} value={u.id}>
                              {u.name || u.email} ({u.email})
                            </option>
                          ))}
                        </select>
                        <Button
                          size="sm"
                          onClick={() => addMember(g.id)}
                          disabled={pending || !memberDraftUser}
                        >
                          Add
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => {
                            setMemberDraftFor(null);
                            setMemberDraftUser("");
                          }}
                          disabled={pending}
                        >
                          <X className="size-4" />
                        </Button>
                      </div>
                    ) : (
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => {
                          setMemberDraftFor(g.id);
                          setMemberDraftUser("");
                        }}
                        disabled={pending || available.length === 0}
                      >
                        <UserPlus className="mr-2 size-4" />
                        Add member
                      </Button>
                    )}
                  </div>
                ) : null}
              </CardContent>
            </Card>
          );
        })}
      </div>
    </div>
  );
}
