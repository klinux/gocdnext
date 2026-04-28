"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import { Copy, KeyRound, Loader2, Plus, Trash2 } from "lucide-react";
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
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  createUserAPIToken,
  revokeUserAPIToken,
  type CreateTokenResponse,
} from "@/server/actions/api-tokens";
import type { APIToken } from "@/server/queries/api-tokens";

type Props = { initial: APIToken[] };

export function UserTokensManager({ initial }: Props) {
  const router = useRouter();
  const [createOpen, setCreateOpen] = useState(false);
  const [showOnce, setShowOnce] = useState<CreateTokenResponse | null>(null);

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <p className="text-sm text-muted-foreground">
          Tokens carry your role + identity. Treat them like passwords.
        </p>
        <Dialog open={createOpen} onOpenChange={setCreateOpen}>
          <DialogTrigger
            render={
              <Button size="sm">
                <Plus className="mr-1 size-4" /> New token
              </Button>
            }
          />

          <CreateDialog
            onCreated={(payload) => {
              setShowOnce(payload);
              setCreateOpen(false);
              router.refresh();
            }}
          />
        </Dialog>
      </div>

      {showOnce ? (
        <ShowOnceCard
          payload={showOnce}
          onDismiss={() => setShowOnce(null)}
        />
      ) : null}

      <div className="overflow-hidden rounded-lg border border-border bg-card">
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
            {initial.length === 0 ? (
              <TableRow>
                <TableCell
                  colSpan={6}
                  className="py-8 text-center text-sm text-muted-foreground"
                >
                  No tokens yet. Click <em>New token</em> to mint one.
                </TableCell>
              </TableRow>
            ) : (
              initial.map((t) => <TokenRow key={t.id} token={t} />)
            )}
          </TableBody>
        </Table>
      </div>
    </div>
  );
}

function TokenRow({ token }: { token: APIToken }) {
  const router = useRouter();
  const [pending, startTransition] = useTransition();
  const onRevoke = () => {
    if (!confirm(`Revoke token "${token.name}"? This cannot be undone.`)) return;
    startTransition(async () => {
      const res = await revokeUserAPIToken(token.id);
      if (res.ok) {
        toast.success(`Token "${token.name}" revoked`);
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

function CreateDialog({
  onCreated,
}: {
  onCreated: (payload: CreateTokenResponse) => void;
}) {
  const [name, setName] = useState("");
  const [expiresAt, setExpiresAt] = useState("");
  const [pending, startTransition] = useTransition();

  const submit = () => {
    startTransition(async () => {
      const res = await createUserAPIToken({
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
        <DialogTitle className="flex items-center gap-2">
          <KeyRound className="size-4" /> New API token
        </DialogTitle>
        <DialogDescription>
          The plaintext is shown <strong>once</strong> — copy it before
          closing the dialog. We hash and never store it.
        </DialogDescription>
      </DialogHeader>
      <div className="space-y-3">
        <div className="space-y-1.5">
          <Label htmlFor="token-name">Name</Label>
          <Input
            id="token-name"
            placeholder="e.g. laptop, ci-script"
            value={name}
            onChange={(e) => setName(e.target.value)}
            disabled={pending}
          />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="token-expires">Expires (optional)</Label>
          <Input
            id="token-expires"
            type="datetime-local"
            value={expiresAt}
            onChange={(e) => setExpiresAt(e.target.value)}
            disabled={pending}
          />
          <p className="text-xs text-muted-foreground">
            Leave empty for no expiry.
          </p>
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

function ShowOnceCard({
  payload,
  onDismiss,
}: {
  payload: CreateTokenResponse;
  onDismiss: () => void;
}) {
  const copy = () => {
    navigator.clipboard.writeText(payload.plaintext).then(() =>
      toast.success("Token copied to clipboard"),
    );
  };
  return (
    <div className="rounded-lg border border-amber-500/40 bg-amber-500/5 p-4">
      <p className="text-sm font-medium text-amber-700 dark:text-amber-400">
        Copy this token now — you won&apos;t see it again.
      </p>
      <div className="mt-3 flex items-center gap-2">
        <code className="flex-1 truncate rounded bg-muted px-2 py-1.5 font-mono text-xs">
          {payload.plaintext}
        </code>
        <Button size="sm" variant="outline" onClick={copy}>
          <Copy className="mr-1 size-3.5" /> Copy
        </Button>
      </div>
      <p className="mt-2 text-xs text-muted-foreground">
        Use it as <code>Authorization: Bearer {payload.plaintext.slice(0, 12)}…</code>
      </p>
      <Button size="sm" variant="ghost" className="mt-3" onClick={onDismiss}>
        Dismiss
      </Button>
    </div>
  );
}
