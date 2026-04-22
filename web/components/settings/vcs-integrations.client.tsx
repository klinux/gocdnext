"use client";

import { useMemo, useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import { GitBranch, KeyRound, Pencil, Plus, RefreshCw, Trash2 } from "lucide-react";
import { toast } from "sonner";

import { StatusPill } from "@/components/shared/status-pill";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
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
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";
import {
  deleteVCSIntegration,
  reloadVCSIntegrations,
  upsertVCSIntegration,
} from "@/server/actions/vcs-integrations";
import type {
  ActiveVCSIntegration,
  ConfiguredVCSIntegration,
} from "@/types/api";

type Props = {
  integrations: ConfiguredVCSIntegration[];
  active: ActiveVCSIntegration[];
};

type FormState = {
  id?: string;
  name: string;
  display_name: string;
  app_id: string; // string input; parsed to number on submit
  private_key: string; // empty = preserve existing on edit
  webhook_secret: string; // empty = preserve existing on edit
  api_base: string;
  enabled: boolean;
};

const EMPTY: FormState = {
  name: "",
  display_name: "",
  app_id: "",
  private_key: "",
  webhook_secret: "",
  api_base: "",
  enabled: true,
};

export function VCSIntegrationsAdminView({ integrations, active }: Props) {
  const router = useRouter();
  const [pending, startTransition] = useTransition();
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState<FormState>(EMPTY);

  // env-only rows are the registry entries that don't have a
  // corresponding DB row — admins can see them but can't edit.
  const envOnly = useMemo(() => {
    const dbNames = new Set(integrations.map((i) => i.name));
    return active.filter((a) => a.source === "env" && !dbNames.has(a.name));
  }, [integrations, active]);

  const startCreate = () => {
    setForm(EMPTY);
    setOpen(true);
  };

  const startEdit = (row: ConfiguredVCSIntegration) => {
    setForm({
      id: row.id,
      name: row.name,
      display_name: row.display_name,
      app_id: row.app_id ? String(row.app_id) : "",
      private_key: "",
      webhook_secret: "",
      api_base: row.api_base ?? "",
      enabled: row.enabled,
    });
    setOpen(true);
  };

  // Rotate dialog state: kept separate from the generic edit form
  // so the narrow "just replace the PEM" workflow doesn't expose
  // AppID / name / webhook secret controls that the user doesn't
  // mean to touch during a key rotation.
  const [rotateRow, setRotateRow] = useState<ConfiguredVCSIntegration | null>(null);
  const [rotatePEM, setRotatePEM] = useState("");

  const startRotate = (row: ConfiguredVCSIntegration) => {
    setRotateRow(row);
    setRotatePEM("");
  };

  const submitRotate = () => {
    if (!rotateRow) return;
    const pem = rotatePEM.trim();
    if (!pem) {
      toast.error("Paste the new private key PEM to rotate.");
      return;
    }
    const appID = rotateRow.app_id;
    if (typeof appID !== "number") {
      toast.error("This integration has no App ID — edit it first.");
      return;
    }
    startTransition(async () => {
      const res = await upsertVCSIntegration({
        name: rotateRow.name,
        kind: "github_app",
        display_name: rotateRow.display_name || undefined,
        app_id: appID,
        private_key: pem,
        // empty webhook_secret preserves the sealed one — rotate
        // is strictly a private-key swap.
        api_base: rotateRow.api_base || undefined,
        enabled: rotateRow.enabled,
      });
      if (res.ok) {
        setRotateRow(null);
        setRotatePEM("");
        const warn =
          typeof res.data.reload_warning === "string"
            ? res.data.reload_warning
            : null;
        toast.success(`Rotated ${rotateRow.name}`, {
          description: warn ? `Saved, but reload warned: ${warn}` : undefined,
        });
        router.refresh();
      } else {
        toast.error(`Rotate failed: ${res.error}`);
      }
    });
  };

  const onSubmit = () => {
    startTransition(async () => {
      const appID = Number(form.app_id);
      if (!Number.isFinite(appID) || appID <= 0) {
        toast.error("App ID must be a positive number.");
        return;
      }
      const res = await upsertVCSIntegration({
        name: form.name,
        kind: "github_app",
        display_name: form.display_name || undefined,
        app_id: appID,
        private_key: form.private_key || undefined,
        webhook_secret: form.webhook_secret || undefined,
        api_base: form.api_base || undefined,
        enabled: form.enabled,
      });
      if (res.ok) {
        setOpen(false);
        const warn =
          typeof res.data.reload_warning === "string"
            ? res.data.reload_warning
            : null;
        toast.success(form.id ? `Updated ${form.name}` : `Created ${form.name}`, {
          description: warn ? `Saved, but reload warned: ${warn}` : undefined,
        });
        router.refresh();
      } else {
        toast.error(`Save failed: ${res.error}`);
      }
    });
  };

  const onDelete = (row: ConfiguredVCSIntegration) => {
    if (!confirm(`Delete integration "${row.name}"?`)) return;
    startTransition(async () => {
      const res = await deleteVCSIntegration({ id: row.id });
      if (res.ok) {
        toast.success(`Deleted ${row.name}`);
        router.refresh();
      } else {
        toast.error(`Delete failed: ${res.error}`);
      }
    });
  };

  const onReload = () => {
    startTransition(async () => {
      const res = await reloadVCSIntegrations();
      if (res.ok) {
        toast.success("Registry reloaded");
        router.refresh();
      } else {
        toast.error(`Reload failed: ${res.error}`);
      }
    });
  };

  return (
    <div className="space-y-4">
      <Card>
        <CardHeader className="flex flex-row items-start justify-between gap-3">
          <div>
            <CardTitle className="text-base flex items-center gap-2">
              <GitBranch className="size-4 text-primary" aria-hidden />
              Source control integrations
            </CardTitle>
            <CardDescription>
              Credentials gocdnext uses to talk back to GitHub (webhook
              auto-register + Checks API). Secrets are AES-GCM sealed with
              your <code className="font-mono">GOCDNEXT_SECRET_KEY</code>.
            </CardDescription>
          </div>
          <div className="flex gap-2 shrink-0">
            <Button variant="outline" size="sm" onClick={onReload} disabled={pending}>
              <RefreshCw className="size-3.5" /> Reload
            </Button>
            <Button size="sm" onClick={startCreate} disabled={pending}>
              <Plus className="size-3.5" /> Add integration
            </Button>
          </div>
        </CardHeader>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Kind</TableHead>
                <TableHead>App ID</TableHead>
                <TableHead>Status</TableHead>
                <TableHead className="w-[120px] text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {integrations.length === 0 && envOnly.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={5} className="py-8 text-center text-sm text-muted-foreground">
                    No integrations yet. Click "Add integration" to configure one.
                  </TableCell>
                </TableRow>
              ) : null}

              {integrations.map((row) => (
                <TableRow key={row.id}>
                  <TableCell className="font-mono text-xs">
                    {row.name}
                    {row.display_name ? (
                      <span className="ml-2 text-muted-foreground">
                        ({row.display_name})
                      </span>
                    ) : null}
                  </TableCell>
                  <TableCell>
                    <span className="text-[10px] font-mono uppercase tracking-wide text-muted-foreground">
                      {row.kind}
                    </span>
                  </TableCell>
                  <TableCell className="font-mono text-xs">{row.app_id ?? "—"}</TableCell>
                  <TableCell>
                    {row.enabled ? (
                      <StatusPill tone="success">enabled</StatusPill>
                    ) : (
                      <StatusPill tone="neutral">disabled</StatusPill>
                    )}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-1">
                      <Button
                        size="icon-sm"
                        variant="ghost"
                        onClick={() => startRotate(row)}
                        aria-label={`Rotate private key for ${row.name}`}
                        title="Rotate private key"
                      >
                        <KeyRound className="size-3.5" />
                      </Button>
                      <Button
                        size="icon-sm"
                        variant="ghost"
                        onClick={() => startEdit(row)}
                        aria-label={`Edit ${row.name}`}
                      >
                        <Pencil className="size-3.5" />
                      </Button>
                      <Button
                        size="icon-sm"
                        variant="ghost"
                        onClick={() => onDelete(row)}
                        aria-label={`Delete ${row.name}`}
                      >
                        <Trash2 className="size-3.5" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}

              {envOnly.map((env) => (
                <TableRow key={`env-${env.name}`} className="text-muted-foreground">
                  <TableCell className="font-mono text-xs">
                    {env.name}
                    <span className="ml-2 text-[10px] uppercase tracking-wide">
                      via env
                    </span>
                  </TableCell>
                  <TableCell>
                    <span className="text-[10px] font-mono uppercase tracking-wide">
                      {env.kind}
                    </span>
                  </TableCell>
                  <TableCell className="font-mono text-xs">{env.app_id ?? "—"}</TableCell>
                  <TableCell>
                    <StatusPill tone="success">active</StatusPill>
                  </TableCell>
                  <TableCell className="text-right">
                    <span className="text-[10px]">env-only</span>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </CardContent>
      </Card>

      <IntegrationDialog
        open={open}
        onOpenChange={setOpen}
        form={form}
        setForm={setForm}
        pending={pending}
        onSubmit={onSubmit}
      />

      <Dialog
        open={rotateRow !== null}
        onOpenChange={(next) => {
          if (!next) {
            setRotateRow(null);
            setRotatePEM("");
          }
        }}
      >
        <DialogContent className="sm:max-w-xl">
          <DialogHeader>
            <DialogTitle>
              Rotate private key
              {rotateRow ? (
                <span className="ml-2 font-mono text-sm text-muted-foreground">
                  {rotateRow.name}
                </span>
              ) : null}
            </DialogTitle>
            <DialogDescription>
              Paste the new PEM from GitHub&apos;s App settings. The old
              key is replaced on save and the VCS registry reloads
              immediately — all other fields (App ID, webhook secret,
              API base) stay as they are.
            </DialogDescription>
          </DialogHeader>

          <Field
            label="New private key PEM"
            hint="-----BEGIN RSA PRIVATE KEY----- …"
          >
            <Textarea
              value={rotatePEM}
              onChange={(e) => setRotatePEM(e.target.value)}
              className="h-40 font-mono text-xs"
              spellCheck={false}
              autoFocus
            />
          </Field>

          <DialogFooter>
            <Button
              variant="outline"
              type="button"
              onClick={() => {
                setRotateRow(null);
                setRotatePEM("");
              }}
              disabled={pending}
            >
              Cancel
            </Button>
            <Button onClick={submitRotate} disabled={pending || !rotatePEM.trim()}>
              <RefreshCw className="size-3.5" />
              {pending ? "Rotating…" : "Rotate key"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function IntegrationDialog({
  open,
  onOpenChange,
  form,
  setForm,
  pending,
  onSubmit,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  form: FormState;
  setForm: (s: FormState) => void;
  pending: boolean;
  onSubmit: () => void;
}) {
  const isEdit = Boolean(form.id);

  const valid = useMemo(() => {
    if (!form.name.trim() || !form.app_id.trim()) return false;
    if (!isEdit && !form.private_key.trim()) return false;
    return true;
  }, [form, isEdit]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <KeyRound className="size-4" />
            {isEdit ? `Edit ${form.name}` : "Add VCS integration"}
          </DialogTitle>
          <DialogDescription>
            {isEdit
              ? "Leave private key / webhook secret blank to keep the stored value."
              : "Configure a GitHub App so gocdnext can install webhooks and post to the Checks API."}
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-3">
          <div className="grid grid-cols-2 gap-3">
            <Field label="Name" hint="lowercase slug">
              <Input
                value={form.name}
                disabled={isEdit}
                onChange={(e) =>
                  setForm({ ...form, name: e.target.value.toLowerCase() })
                }
                placeholder="primary-gh"
              />
            </Field>
            <Field label="Display name">
              <Input
                value={form.display_name}
                onChange={(e) => setForm({ ...form, display_name: e.target.value })}
                placeholder="gocdnext (production)"
              />
            </Field>
          </div>

          <Field label="App ID" hint="numeric, from github.com/settings/apps/<name>">
            <Input
              value={form.app_id}
              inputMode="numeric"
              onChange={(e) =>
                setForm({ ...form, app_id: e.target.value.replace(/[^0-9]/g, "") })
              }
              placeholder="123456"
            />
          </Field>

          <Field
            label={isEdit ? "Private key PEM (leave blank to keep)" : "Private key PEM"}
            hint="-----BEGIN RSA PRIVATE KEY----- …"
          >
            <Textarea
              value={form.private_key}
              onChange={(e) => setForm({ ...form, private_key: e.target.value })}
              placeholder={isEdit ? "•••• (stored; retype to rotate) ••••" : ""}
              className="h-32 font-mono text-xs"
              spellCheck={false}
            />
          </Field>

          <Field
            label={isEdit ? "Webhook secret (leave blank to keep)" : "Webhook secret (optional)"}
          >
            <Input
              type="password"
              value={form.webhook_secret}
              onChange={(e) => setForm({ ...form, webhook_secret: e.target.value })}
              placeholder={isEdit ? "••••" : "shared HMAC with repo webhooks"}
            />
          </Field>

          <Field label="API base (optional; GitHub Enterprise only)">
            <Input
              value={form.api_base}
              onChange={(e) => setForm({ ...form, api_base: e.target.value })}
              placeholder="https://github.example.com/api/v3"
            />
          </Field>

          <div className="flex items-center gap-2">
            <input
              id="vcs-enabled"
              type="checkbox"
              checked={form.enabled}
              onChange={(e) => setForm({ ...form, enabled: e.target.checked })}
              className="size-3.5 accent-primary"
            />
            <Label htmlFor="vcs-enabled" className="text-xs">
              Enabled (registry picks this one up on reload)
            </Label>
          </div>
        </div>

        <DialogFooter>
          <Button
            variant="ghost"
            onClick={() => onOpenChange(false)}
            disabled={pending}
          >
            Cancel
          </Button>
          <Button onClick={onSubmit} disabled={!valid || pending}>
            {isEdit ? "Save" : "Create"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function Field({
  label,
  hint,
  children,
  className,
}: {
  label: string;
  hint?: string;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn("space-y-1", className)}>
      <Label className="text-xs text-muted-foreground">
        {label}
        {hint ? (
          <span className="ml-1 text-[10px] normal-case text-muted-foreground/70">
            · {hint}
          </span>
        ) : null}
      </Label>
      {children}
    </div>
  );
}
