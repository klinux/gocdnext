"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import { Loader2, Plus, UserPlus } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { createLocalUser } from "@/server/actions/users";

type Role = "admin" | "maintainer" | "viewer";

export function NewUserDialog() {
  const router = useRouter();
  const [open, setOpen] = useState(false);
  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const [role, setRole] = useState<Role>("maintainer");
  const [password, setPassword] = useState("");
  const [pending, startTransition] = useTransition();

  const reset = () => {
    setEmail("");
    setName("");
    setRole("maintainer");
    setPassword("");
  };

  const submit = () => {
    startTransition(async () => {
      const res = await createLocalUser({
        email: email.trim(),
        name: name.trim(),
        role,
        password,
      });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      toast.success(`User ${email.trim()} created`);
      reset();
      setOpen(false);
      router.refresh();
    });
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (!next) reset();
      }}
    >
      <DialogTrigger
        render={
          <Button size="sm">
            <Plus className="mr-1 size-4" /> New user
          </Button>
        }
      />
      <DialogContent>
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <UserPlus className="size-4" /> New local user
          </DialogTitle>
          <DialogDescription>
            Provisions a password-backed account. OIDC users are created
            automatically on first login — use this for local accounts only.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-3">
          <div className="space-y-1.5">
            <Label htmlFor="new-user-email">Email</Label>
            <Input
              id="new-user-email"
              type="email"
              placeholder="alice@example.com"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              disabled={pending}
              autoFocus
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="new-user-name">Name</Label>
            <Input
              id="new-user-name"
              placeholder="Alice"
              value={name}
              onChange={(e) => setName(e.target.value)}
              disabled={pending}
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="new-user-role">Role</Label>
            <select
              id="new-user-role"
              value={role}
              onChange={(e) => setRole(e.target.value as Role)}
              disabled={pending}
              className="h-9 w-full rounded-md border bg-background px-2 text-sm"
            >
              <option value="viewer">viewer</option>
              <option value="maintainer">maintainer</option>
              <option value="admin">admin</option>
            </select>
            <p className="text-xs text-muted-foreground">
              admin ≥ maintainer ≥ viewer. Promote later from the table.
            </p>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="new-user-password">Initial password</Label>
            <Input
              id="new-user-password"
              type="password"
              placeholder="At least 8 characters"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              disabled={pending}
            />
            <p className="text-xs text-muted-foreground">
              Share it via your team&apos;s password manager. The user can
              rotate it from <em>Account → Change password</em> after first
              login.
            </p>
          </div>
        </div>
        <DialogFooter>
          <Button
            onClick={submit}
            disabled={!email.trim() || password.length < 8 || pending}
          >
            {pending ? <Loader2 className="mr-2 size-4 animate-spin" /> : null}
            Create user
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
