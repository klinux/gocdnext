"use client";

import { useState, useTransition } from "react";
import { toast } from "sonner";

import { cn } from "@/lib/utils";
import { setUserRole } from "@/server/actions/users";

type Role = "admin" | "maintainer" | "viewer";
const ROLES: Role[] = ["admin", "maintainer", "viewer"];

type Props = {
  userID: string;
  email: string;
  currentRole: string;
  // self=true when the row represents the authenticated admin —
  // the store refuses self-demotion anyway, but disabling the
  // dropdown up front saves the operator a confusing 403 toast.
  self?: boolean;
};

// Minimal native <select> styled like shadcn. Kept simple because
// three fixed options don't justify the full shadcn Select UX
// (search, virtualisation, etc.). Optimistic update: we flip the
// value locally, fire the action, revert + toast on failure.
export function RoleSelect({ userID, email, currentRole, self }: Props) {
  const [value, setValue] = useState<string>(currentRole);
  const [pending, startTransition] = useTransition();

  function onChange(next: string) {
    if (!ROLES.includes(next as Role) || next === value) return;
    const prev = value;
    setValue(next);
    startTransition(async () => {
      const res = await setUserRole({ userID, role: next as Role });
      if (!res.ok) {
        setValue(prev);
        toast.error(`${email}: ${res.error}`);
        return;
      }
      toast.success(`${email} → ${next}`);
    });
  }

  return (
    <select
      value={value}
      onChange={(e) => onChange(e.target.value)}
      disabled={self || pending}
      aria-label={`Role for ${email}`}
      className={cn(
        "h-8 rounded-md border border-input bg-background px-2 text-sm",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
        "disabled:cursor-not-allowed disabled:opacity-60",
      )}
    >
      {ROLES.map((r) => (
        <option key={r} value={r}>
          {r}
        </option>
      ))}
    </select>
  );
}
