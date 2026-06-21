"use client";

import { useState, useTransition } from "react";
import { toast } from "sonner";

import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { setUserRole } from "@/server/actions/users";

type Role = "admin" | "maintainer" | "viewer";
const ROLES: Role[] = ["admin", "maintainer", "viewer"];
// base-ui's Select.Value renders the raw value unless `items` maps it;
// role labels happen to equal their values, but the map keeps the
// trigger label-driven instead of value-driven.
const ROLE_LABELS: Record<string, string> = {
  admin: "admin",
  maintainer: "maintainer",
  viewer: "viewer",
};

type Props = {
  userID: string;
  email: string;
  currentRole: string;
  // self=true when the row represents the authenticated admin —
  // the store refuses self-demotion anyway, but disabling the
  // dropdown up front saves the operator a confusing 403 toast.
  self?: boolean;
};

// shadcn Select (base-ui) for the three fixed roles. Optimistic
// update: we flip the value locally, fire the action, revert + toast
// on failure.
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
    <Select
      items={ROLE_LABELS}
      value={value}
      disabled={self || pending}
      onValueChange={(v) => {
        if (typeof v === "string") onChange(v);
      }}
    >
      <SelectTrigger aria-label={`Role for ${email}`} className="w-32">
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        {ROLES.map((r) => (
          <SelectItem key={r} value={r}>
            {r}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}
