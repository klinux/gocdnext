"use client";

import { useMemo, useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import { KeyRound, Pencil, Plus, RefreshCw, Trash2 } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
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
import {
  deleteAuthProvider,
  reloadAuthProviders,
  upsertAuthProvider,
} from "@/server/actions/auth-providers";
import type { ConfiguredAuthProvider } from "@/types/api";

type Props = {
  enabled: boolean;
  providers: ConfiguredAuthProvider[];
  envOnly: string[];
};

type FormState = {
  id?: string; // present on edit
  name: string;
  kind: "github" | "oidc";
  display_name: string;
  client_id: string;
  client_secret: string; // empty on edit = preserve
  issuer: string;
  github_api_base: string;
  enabled: boolean;
};

const EMPTY: FormState = {
  name: "",
  kind: "oidc",
  display_name: "",
  client_id: "",
  client_secret: "",
  issuer: "",
  github_api_base: "",
  enabled: true,
};

export function AuthProvidersAdminView({ enabled, providers, envOnly }: Props) {
  const router = useRouter();
  const [pending, startTransition] = useTransition();
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState<FormState>(EMPTY);

  const startCreate = () => {
    setForm(EMPTY);
    setOpen(true);
  };

  const startEdit = (p: ConfiguredAuthProvider) => {
    setForm({
      id: p.id,
      name: p.name,
      kind: p.kind,
      display_name: p.display_name,
      client_id: p.client_id,
      client_secret: "", // blank means "keep"
      issuer: p.issuer ?? "",
      github_api_base: p.github_api_base ?? "",
      enabled: p.enabled,
    });
    setOpen(true);
  };

  const onSubmit = () => {
    startTransition(async () => {
      const res = await upsertAuthProvider({
        name: form.name,
        kind: form.kind,
        display_name: form.display_name || undefined,
        client_id: form.client_id,
        client_secret: form.client_secret || undefined,
        issuer: form.issuer || undefined,
        github_api_base: form.github_api_base || undefined,
        enabled: form.enabled,
      });
      if (res.ok) {
        setOpen(false);
        const warning =
          typeof res.data.reload_warning === "string"
            ? res.data.reload_warning
            : null;
        toast.success(
          form.id ? `Updated ${form.name}` : `Created ${form.name}`,
          { description: warning ? `Saved, but reload warned: ${warning}` : undefined },
        );
        router.refresh();
      } else {
        toast.error(`Save failed: ${res.error}`);
      }
    });
  };

  const onDelete = (p: ConfiguredAuthProvider) => {
    if (!confirm(`Delete provider "${p.name}"?`)) return;
    startTransition(async () => {
      const res = await deleteAuthProvider({ id: p.id });
      if (res.ok) {
        toast.success(`Deleted ${p.name}`);
        router.refresh();
      } else {
        toast.error(`Delete failed: ${res.error}`);
      }
    });
  };

  const onReload = () => {
    startTransition(async () => {
      const res = await reloadAuthProviders();
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
      {!enabled ? (
        <Card className="border-status-warning/40 bg-status-warning-bg">
          <CardHeader>
            <CardTitle className="text-sm">Auth is globally disabled</CardTitle>
            <CardDescription>
              Set <code className="font-mono">GOCDNEXT_AUTH_ENABLED=true</code>{" "}
              and restart the server to enforce sessions. Providers can still
              be configured here — they'll activate once enforcement is on.
            </CardDescription>
          </CardHeader>
        </Card>
      ) : null}

      <div className="flex items-center justify-between">
        <p className="text-sm text-muted-foreground">
          {providers.length} stored provider{providers.length === 1 ? "" : "s"}
          {envOnly.length > 0 ? (
            <>
              {" · "}
              <span>{envOnly.length} env-only (read-only)</span>
            </>
          ) : null}
        </p>
        <div className="flex gap-2">
          <Button variant="outline" size="sm" onClick={onReload} disabled={pending}>
            <RefreshCw className="size-3.5" /> Reload
          </Button>
          <Button size="sm" onClick={startCreate} disabled={pending}>
            <Plus className="size-3.5" /> Add provider
          </Button>
        </div>
      </div>

      <Card>
        <CardContent className="p-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Kind</TableHead>
                <TableHead>Client ID</TableHead>
                <TableHead>Enabled</TableHead>
                <TableHead className="w-[120px] text-right">Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {providers.length === 0 && envOnly.length === 0 ? (
                <TableRow>
                  <TableCell colSpan={5} className="py-8 text-center text-sm text-muted-foreground">
                    No providers yet. Click "Add provider" to configure one.
                  </TableCell>
                </TableRow>
              ) : null}

              {providers.map((p) => (
                <TableRow key={p.id}>
                  <TableCell className="font-mono text-xs">
                    {p.name}
                    {p.display_name ? (
                      <span className="ml-2 text-muted-foreground">
                        ({p.display_name})
                      </span>
                    ) : null}
                  </TableCell>
                  <TableCell>
                    <Badge variant="outline" className="font-mono text-[10px] uppercase">
                      {p.kind}
                    </Badge>
                  </TableCell>
                  <TableCell className="max-w-[200px] truncate font-mono text-xs">
                    {p.client_id}
                  </TableCell>
                  <TableCell>
                    {p.enabled ? (
                      <Badge variant="secondary">on</Badge>
                    ) : (
                      <Badge variant="outline">off</Badge>
                    )}
                  </TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-1">
                      <Button
                        size="icon-sm"
                        variant="ghost"
                        onClick={() => startEdit(p)}
                        aria-label={`Edit ${p.name}`}
                      >
                        <Pencil className="size-3.5" />
                      </Button>
                      <Button
                        size="icon-sm"
                        variant="ghost"
                        onClick={() => onDelete(p)}
                        aria-label={`Delete ${p.name}`}
                      >
                        <Trash2 className="size-3.5" />
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}

              {envOnly.map((name) => (
                <TableRow key={`env-${name}`} className="text-muted-foreground">
                  <TableCell className="font-mono text-xs">
                    {name}
                    <span className="ml-2 text-[10px] uppercase tracking-wide">
                      via env
                    </span>
                  </TableCell>
                  <TableCell>—</TableCell>
                  <TableCell>—</TableCell>
                  <TableCell>
                    <Badge variant="outline">on</Badge>
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

      <ProviderDialog
        open={open}
        onOpenChange={setOpen}
        form={form}
        setForm={setForm}
        pending={pending}
        onSubmit={onSubmit}
      />
    </div>
  );
}

function ProviderDialog({
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
  const kindOIDC = form.kind === "oidc";

  // Computed validity for the primary button. Secret is required on
  // create, optional on edit (empty = keep existing ciphertext).
  const valid = useMemo(() => {
    if (!form.name.trim() || !form.client_id.trim()) return false;
    if (!isEdit && !form.client_secret.trim()) return false;
    if (kindOIDC && !form.issuer.trim()) return false;
    return true;
  }, [form, isEdit, kindOIDC]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2">
            <KeyRound className="size-4" />
            {isEdit ? `Edit ${form.name}` : "Add auth provider"}
          </DialogTitle>
          <DialogDescription>
            {isEdit
              ? "Leave client secret blank to keep the existing value."
              : "Configure a GitHub OAuth app or any OIDC provider (Google, Keycloak, corporate SSO)."}
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-3">
          <div className="grid grid-cols-2 gap-3">
            <Field label="Name">
              <Input
                value={form.name}
                disabled={isEdit}
                onChange={(e) => setForm({ ...form, name: e.target.value.toLowerCase() })}
                placeholder="google"
              />
            </Field>
            <Field label="Kind">
              <select
                value={form.kind}
                onChange={(e) =>
                  setForm({ ...form, kind: e.target.value as FormState["kind"] })
                }
                className="h-8 w-full rounded-md border bg-background px-2 text-sm"
              >
                <option value="oidc">oidc</option>
                <option value="github">github</option>
              </select>
            </Field>
          </div>

          <Field label="Display name">
            <Input
              value={form.display_name}
              onChange={(e) => setForm({ ...form, display_name: e.target.value })}
              placeholder="Google Workspace"
            />
          </Field>

          <Field label="Client ID">
            <Input
              value={form.client_id}
              onChange={(e) => setForm({ ...form, client_id: e.target.value })}
              placeholder="xxxxx.apps.googleusercontent.com"
            />
          </Field>

          <Field
            label={isEdit ? "Client secret (leave blank to keep)" : "Client secret"}
          >
            <Input
              type="password"
              value={form.client_secret}
              onChange={(e) => setForm({ ...form, client_secret: e.target.value })}
              placeholder={isEdit ? "••••••••" : ""}
            />
          </Field>

          {kindOIDC ? (
            <Field label="Issuer URL">
              <Input
                value={form.issuer}
                onChange={(e) => setForm({ ...form, issuer: e.target.value })}
                placeholder="https://accounts.google.com"
              />
            </Field>
          ) : (
            <Field label="GitHub API base (optional, for Enterprise)">
              <Input
                value={form.github_api_base}
                onChange={(e) => setForm({ ...form, github_api_base: e.target.value })}
                placeholder="https://github.example.com/api/v3"
              />
            </Field>
          )}

          <div className="flex items-center gap-2">
            <input
              id="provider-enabled"
              type="checkbox"
              checked={form.enabled}
              onChange={(e) => setForm({ ...form, enabled: e.target.checked })}
              className="size-3.5 accent-primary"
            />
            <Label htmlFor="provider-enabled" className="text-xs">
              Enabled (appears on the login page)
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

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="space-y-1">
      <Label className="text-xs text-muted-foreground">{label}</Label>
      {children}
    </div>
  );
}
