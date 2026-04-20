"use client";

import { useState, useTransition } from "react";
import { KeyRound } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { changeOwnPassword } from "@/server/actions/account";

export function ChangePasswordForm() {
  const [pending, startTransition] = useTransition();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState<string | null>(null);

  const onSubmit = (e: React.FormEvent<HTMLFormElement>) => {
    e.preventDefault();
    setError(null);
    if (next !== confirm) {
      setError("New password and confirmation don't match.");
      return;
    }
    if (next.length < 8) {
      setError("Password must be at least 8 characters.");
      return;
    }
    startTransition(async () => {
      const res = await changeOwnPassword({
        current_password: current,
        new_password: next,
      });
      if (res.ok) {
        toast.success("Password updated");
        setCurrent("");
        setNext("");
        setConfirm("");
      } else {
        setError(res.error);
      }
    });
  };

  return (
    <form onSubmit={onSubmit} className="space-y-3 max-w-sm">
      <Field id="current" label="Current password" value={current} onChange={setCurrent} disabled={pending} />
      <Field id="next" label="New password" value={next} onChange={setNext} disabled={pending} />
      <Field id="confirm" label="Confirm new password" value={confirm} onChange={setConfirm} disabled={pending} />
      {error ? (
        <p role="alert" className="text-xs text-destructive">
          {error}
        </p>
      ) : null}
      <Button type="submit" disabled={pending || !current || !next || !confirm}>
        <KeyRound className="size-3.5" />
        {pending ? "Saving…" : "Change password"}
      </Button>
    </form>
  );
}

function Field({
  id,
  label,
  value,
  onChange,
  disabled,
}: {
  id: string;
  label: string;
  value: string;
  onChange: (v: string) => void;
  disabled: boolean;
}) {
  return (
    <div className="space-y-1">
      <Label htmlFor={id} className="text-xs text-muted-foreground">
        {label}
      </Label>
      <Input
        id={id}
        type="password"
        autoComplete="new-password"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        disabled={disabled}
        required
      />
    </div>
  );
}
