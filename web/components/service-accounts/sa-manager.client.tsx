"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import {
  Bot,
  ChevronDown,
  ChevronRight,
  Copy,
  KeyRound,
  Loader2,
  Pencil,
  Plus,
  Power,
  Trash2,
} from "lucide-react";
import { toast } from "sonner";

import { cn } from "@/lib/utils";
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
import { Textarea } from "@/components/ui/textarea";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  createSAToken,
  createServiceAccount,
  deleteServiceAccount,
  revokeSAToken,
  setServiceAccountDisabled,
  updateServiceAccount,
  type CreateTokenResponse,
} from "@/server/actions/api-tokens";
import type { APIToken, ServiceAccount } from "@/server/queries/api-tokens";

type Props = {
  initial: ServiceAccount[];
  // tokensBySA is fetched server-side in the page so the manager has
  // it on first paint without a per-row client roundtrip.
  tokensBySA: Record<string, APIToken[]>;
};

export function ServiceAccountsManager({ initial, tokensBySA }: Props) {
  const [createOpen, setCreateOpen] = useState(false);
  const router = useRouter();

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <p className="text-sm text-muted-foreground">
          Machine identities for CI, scripts, and other automation. Each
          SA can hold multiple tokens — rotate by creating a new one
          before revoking the old.
        </p>
        <Dialog open={createOpen} onOpenChange={setCreateOpen}>
          <DialogTrigger
            render={
              <Button size="sm">
                <Plus className="mr-1 size-4" /> New service account
              </Button>
            }
          />

          <CreateSADialog
            onCreated={() => {
              setCreateOpen(false);
              router.refresh();
            }}
          />
        </Dialog>
      </div>

      <div className="space-y-3">
        {initial.length === 0 ? (
          <div className="rounded-lg border border-dashed border-border py-12 text-center text-sm text-muted-foreground">
            No service accounts yet.
          </div>
        ) : (
          initial.map((sa) => (
            <ServiceAccountCard
              key={sa.id}
              sa={sa}
              tokens={tokensBySA[sa.id] ?? []}
            />
          ))
        )}
      </div>
    </div>
  );
}

function ServiceAccountCard({
  sa,
  tokens,
}: {
  sa: ServiceAccount;
  tokens: APIToken[];
}) {
  const [open, setOpen] = useState(false);
  const [editOpen, setEditOpen] = useState(false);
  const [tokenOpen, setTokenOpen] = useState(false);
  const [showOnce, setShowOnce] = useState<CreateTokenResponse | null>(null);
  const [pending, startTransition] = useTransition();
  const router = useRouter();

  const onToggleDisabled = () => {
    startTransition(async () => {
      const res = await setServiceAccountDisabled(sa.id, !sa.disabled_at);
      if (res.ok) {
        toast.success(sa.disabled_at ? "Re-enabled" : "Disabled");
        router.refresh();
      } else {
        toast.error(res.error);
      }
    });
  };
  const onDelete = () => {
    if (!confirm(`Delete "${sa.name}" and revoke all its tokens?`)) return;
    startTransition(async () => {
      const res = await deleteServiceAccount(sa.id);
      if (res.ok) {
        toast.success("Deleted");
        router.refresh();
      } else {
        toast.error(res.error);
      }
    });
  };

  return (
    <div className="rounded-lg border border-border bg-card">
      <header
        className="flex items-center gap-3 p-4"
        aria-disabled={!!sa.disabled_at}
      >
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          className="text-muted-foreground hover:text-foreground"
          aria-label={open ? "Collapse" : "Expand"}
        >
          {open ? (
            <ChevronDown className="size-4" />
          ) : (
            <ChevronRight className="size-4" />
          )}
        </button>
        <Bot
          className={cn(
            "size-5",
            sa.disabled_at ? "text-muted-foreground/60" : "text-primary",
          )}
        />
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <h3 className="font-mono text-sm font-semibold">{sa.name}</h3>
            <span className="rounded bg-muted px-1.5 py-0.5 text-[10px] uppercase tracking-wide text-muted-foreground">
              {sa.role}
            </span>
            {sa.disabled_at ? (
              <span className="rounded bg-muted-foreground/20 px-1.5 py-0.5 text-[10px] uppercase tracking-wide text-muted-foreground">
                disabled
              </span>
            ) : null}
          </div>
          {sa.description ? (
            <p className="mt-0.5 text-xs text-muted-foreground">{sa.description}</p>
          ) : null}
        </div>
        <div className="flex items-center gap-1">
          <Dialog open={tokenOpen} onOpenChange={setTokenOpen}>
            <DialogTrigger
              render={
                <Button size="sm" variant="outline">
                  <KeyRound className="mr-1 size-3.5" /> New token
                </Button>
              }
            />

            <CreateSATokenDialog
              saID={sa.id}
              onCreated={(p) => {
                setShowOnce(p);
                setTokenOpen(false);
                setOpen(true);
                router.refresh();
              }}
            />
          </Dialog>
          <Dialog open={editOpen} onOpenChange={setEditOpen}>
            <DialogTrigger
              render={
                <Button size="icon" variant="ghost" aria-label="Edit">
                  <Pencil className="size-4" />
                </Button>
              }
            />

            <EditSADialog sa={sa} onSaved={() => setEditOpen(false)} />
          </Dialog>
          <Button
            size="icon"
            variant="ghost"
            onClick={onToggleDisabled}
            disabled={pending}
            aria-label={sa.disabled_at ? "Re-enable" : "Disable"}
          >
            <Power className="size-4" />
          </Button>
          <Button
            size="icon"
            variant="ghost"
            onClick={onDelete}
            disabled={pending}
            aria-label="Delete"
          >
            <Trash2 className="size-4 text-destructive" />
          </Button>
        </div>
      </header>

      {showOnce ? (
        <div className="border-t border-border bg-amber-500/5 px-4 py-3">
          <p className="text-sm font-medium text-amber-700 dark:text-amber-400">
            Copy this token now — you won&apos;t see it again.
          </p>
          <div className="mt-2 flex items-center gap-2">
            <code className="flex-1 truncate rounded bg-muted px-2 py-1.5 font-mono text-xs">
              {showOnce.plaintext}
            </code>
            <Button
              size="sm"
              variant="outline"
              onClick={() => {
                navigator.clipboard.writeText(showOnce.plaintext).then(() =>
                  toast.success("Copied"),
                );
              }}
            >
              <Copy className="mr-1 size-3.5" /> Copy
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setShowOnce(null)}>
              Dismiss
            </Button>
          </div>
        </div>
      ) : null}

      {open ? <SATokensList saID={sa.id} tokens={tokens} /> : null}
    </div>
  );
}

function SATokensList({ saID, tokens }: { saID: string; tokens: APIToken[] }) {
  if (tokens.length === 0) {
    return (
      <p className="border-t border-border px-4 py-3 text-xs text-muted-foreground">
        No tokens yet. Use <em>New token</em> above to mint one.
      </p>
    );
  }
  return (
    <div className="border-t border-border">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            <TableHead className="w-32">Prefix</TableHead>
            <TableHead className="w-40">Expires</TableHead>
            <TableHead className="w-40">Last used</TableHead>
            <TableHead className="w-32">Status</TableHead>
            <TableHead className="w-20 text-right">&nbsp;</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {tokens.map((t) => (
            <SATokenRow key={t.id} saID={saID} token={t} />
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

function SATokenRow({ saID, token }: { saID: string; token: APIToken }) {
  const router = useRouter();
  const [pending, startTransition] = useTransition();
  const onRevoke = () => {
    if (!confirm(`Revoke token "${token.name}"?`)) return;
    startTransition(async () => {
      const res = await revokeSAToken(saID, token.id);
      if (res.ok) {
        toast.success("Revoked");
        router.refresh();
      } else {
        toast.error(res.error);
      }
    });
  };
  const status = token.revoked_at
    ? "revoked"
    : token.expires_at && new Date(token.expires_at).getTime() < Date.now()
      ? "expired"
      : "active";
  const statusClass = {
    active: "text-emerald-500",
    expired: "text-amber-500",
    revoked: "text-muted-foreground line-through",
  }[status];
  return (
    <TableRow className="text-xs">
      <TableCell className="font-medium">{token.name}</TableCell>
      <TableCell className="font-mono">{token.prefix}…</TableCell>
      <TableCell>{token.expires_at ? new Date(token.expires_at).toLocaleString() : "—"}</TableCell>
      <TableCell>
        {token.last_used_at
          ? new Date(token.last_used_at).toLocaleString()
          : "—"}
      </TableCell>
      <TableCell className={cn("uppercase tracking-wide", statusClass)}>
        {status}
      </TableCell>
      <TableCell className="text-right">
        {status !== "revoked" ? (
          <Button
            variant="ghost"
            size="icon"
            disabled={pending}
            onClick={onRevoke}
            aria-label="Revoke"
          >
            {pending ? (
              <Loader2 className="size-4 animate-spin" />
            ) : (
              <Trash2 className="size-4 text-destructive" />
            )}
          </Button>
        ) : null}
      </TableCell>
    </TableRow>
  );
}

function CreateSADialog({ onCreated }: { onCreated: () => void }) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [role, setRole] = useState<"admin" | "maintainer" | "viewer">("maintainer");
  const [pending, startTransition] = useTransition();

  const submit = () => {
    startTransition(async () => {
      const res = await createServiceAccount({
        name: name.trim(),
        description,
        role,
      });
      if (res.ok) {
        onCreated();
        setName("");
        setDescription("");
        setRole("maintainer");
      } else {
        toast.error(res.error);
      }
    });
  };

  return (
    <DialogContent>
      <DialogHeader>
        <DialogTitle className="flex items-center gap-2">
          <Bot className="size-4" /> New service account
        </DialogTitle>
        <DialogDescription>
          Machine identity. Pick the lowest role that lets it do its
          job — maintainer covers most CI use cases.
        </DialogDescription>
      </DialogHeader>
      <div className="space-y-3">
        <div className="space-y-1.5">
          <Label htmlFor="sa-name">Name</Label>
          <Input
            id="sa-name"
            placeholder="e.g. ci-bot, terraform-deploy"
            value={name}
            onChange={(e) => setName(e.target.value)}
            disabled={pending}
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="sa-desc">Description</Label>
          <Textarea
            id="sa-desc"
            rows={2}
            placeholder="What does this SA do?"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            disabled={pending}
          />
        </div>
        <div className="space-y-1.5">
          <Label>Role</Label>
          <div className="flex flex-wrap gap-2">
            {(["admin", "maintainer", "viewer"] as const).map((r) => (
              <button
                key={r}
                type="button"
                onClick={() => setRole(r)}
                className={cn(
                  "rounded-md border px-3 py-1.5 text-sm transition-colors",
                  r === role
                    ? "border-primary bg-primary/10 text-primary"
                    : "border-border bg-background hover:bg-muted",
                )}
              >
                {r}
              </button>
            ))}
          </div>
        </div>
      </div>
      <DialogFooter>
        <Button onClick={submit} disabled={!name.trim() || pending}>
          {pending ? (
            <Loader2 className="mr-2 size-4 animate-spin" />
          ) : null}
          Create
        </Button>
      </DialogFooter>
    </DialogContent>
  );
}

function EditSADialog({
  sa,
  onSaved,
}: {
  sa: ServiceAccount;
  onSaved: () => void;
}) {
  const router = useRouter();
  const [description, setDescription] = useState(sa.description);
  const [role, setRole] = useState<"admin" | "maintainer" | "viewer">(sa.role);
  const [pending, startTransition] = useTransition();

  const submit = () => {
    startTransition(async () => {
      const res = await updateServiceAccount({
        id: sa.id,
        description,
        role,
      });
      if (res.ok) {
        toast.success("Saved");
        onSaved();
        router.refresh();
      } else {
        toast.error(res.error);
      }
    });
  };

  return (
    <DialogContent>
      <DialogHeader>
        <DialogTitle>Edit {sa.name}</DialogTitle>
      </DialogHeader>
      <div className="space-y-3">
        <div className="space-y-1.5">
          <Label htmlFor={`edit-desc-${sa.id}`}>Description</Label>
          <Textarea
            id={`edit-desc-${sa.id}`}
            rows={2}
            value={description}
            onChange={(e) => setDescription(e.target.value)}
            disabled={pending}
          />
        </div>
        <div className="space-y-1.5">
          <Label>Role</Label>
          <div className="flex flex-wrap gap-2">
            {(["admin", "maintainer", "viewer"] as const).map((r) => (
              <button
                key={r}
                type="button"
                onClick={() => setRole(r)}
                className={cn(
                  "rounded-md border px-3 py-1.5 text-sm transition-colors",
                  r === role
                    ? "border-primary bg-primary/10 text-primary"
                    : "border-border bg-background hover:bg-muted",
                )}
              >
                {r}
              </button>
            ))}
          </div>
        </div>
      </div>
      <DialogFooter>
        <Button onClick={submit} disabled={pending}>
          {pending ? (
            <Loader2 className="mr-2 size-4 animate-spin" />
          ) : null}
          Save
        </Button>
      </DialogFooter>
    </DialogContent>
  );
}

function CreateSATokenDialog({
  saID,
  onCreated,
}: {
  saID: string;
  onCreated: (payload: CreateTokenResponse) => void;
}) {
  const [name, setName] = useState("");
  const [expiresAt, setExpiresAt] = useState("");
  const [pending, startTransition] = useTransition();

  const submit = () => {
    startTransition(async () => {
      const res = await createSAToken({
        saID,
        name: name.trim(),
        expires_at: expiresAt || null,
      });
      if (res.ok) {
        onCreated(res.data);
        setName("");
        setExpiresAt("");
      } else {
        toast.error(res.error);
      }
    });
  };

  return (
    <DialogContent>
      <DialogHeader>
        <DialogTitle>New token</DialogTitle>
        <DialogDescription>
          Plaintext is shown once. Copy it before closing.
        </DialogDescription>
      </DialogHeader>
      <div className="space-y-3">
        <div className="space-y-1.5">
          <Label htmlFor={`sat-name-${saID}`}>Name</Label>
          <Input
            id={`sat-name-${saID}`}
            placeholder="e.g. primary, rotation-2026q2"
            value={name}
            onChange={(e) => setName(e.target.value)}
            disabled={pending}
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor={`sat-exp-${saID}`}>Expires (optional)</Label>
          <Input
            id={`sat-exp-${saID}`}
            type="datetime-local"
            value={expiresAt}
            onChange={(e) => setExpiresAt(e.target.value)}
            disabled={pending}
          />
        </div>
      </div>
      <DialogFooter>
        <Button onClick={submit} disabled={!name.trim() || pending}>
          {pending ? (
            <Loader2 className="mr-2 size-4 animate-spin" />
          ) : null}
          Create
        </Button>
      </DialogFooter>
    </DialogContent>
  );
}
