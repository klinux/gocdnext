"use client";

import { Loader2, X } from "lucide-react";

import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Textarea } from "@/components/ui/textarea";
import { clusterAuthTypes, type ClusterAuthType } from "@/lib/clusters";
import type { AdminCluster } from "@/server/queries/admin";

export type ProjectOption = { id: string; name: string; slug: string };

// FormDraft owns the editor's local state. `id === null` means "create".
// credential is WRITE-ONLY (the list endpoint never returns it): on edit
// it starts blank and we send the preserve sentinel when the admin leaves
// it untouched. ca_cert is a PUBLIC cert the server DOES echo, so on edit
// it is PREFILLED from the stored value and re-sent — a metadata-only edit
// must not drop it and degrade token auth to insecure TLS.
export type FormDraft = {
  id: string | null;
  name: string;
  description: string;
  auth_type: ClusterAuthType;
  api_server: string;
  ca_cert: string;
  credential: string;
  allowedProjects: string[];
  allowDeclarativeTargets: boolean;
};

export const AUTH_LABELS: Record<ClusterAuthType, string> = {
  kubeconfig: "kubeconfig",
  token: "token",
  in_cluster: "in-cluster",
};

export function blankForm(): FormDraft {
  return {
    id: null,
    name: "",
    description: "",
    auth_type: "kubeconfig",
    api_server: "",
    ca_cert: "",
    credential: "",
    allowedProjects: [],
    allowDeclarativeTargets: false,
  };
}

export function clusterToDraft(c: AdminCluster): FormDraft {
  return {
    id: c.id,
    name: c.name,
    description: c.description,
    auth_type: c.auth_type,
    api_server: c.api_server,
    // CA cert is public and echoed by the server → prefill it so the
    // edit re-sends it. The credential is write-only → starts blank.
    ca_cert: c.ca_cert ?? "",
    credential: "",
    allowedProjects: [...(c.allowed_projects ?? [])],
    allowDeclarativeTargets: c.allow_declarative_targets ?? false,
  };
}

function Field({
  label,
  hint,
  required,
  htmlFor,
  children,
}: {
  label: string;
  hint?: string;
  required?: boolean;
  htmlFor?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={htmlFor}>
        {label}
        {required ? <span className="text-destructive"> *</span> : null}
      </Label>
      {children}
      {hint ? <p className="text-xs text-muted-foreground">{hint}</p> : null}
    </div>
  );
}

// ClusterForm is the editor body. Split out of the manager so each file
// stays under the 400-line cap and the manager stays focused on
// list/dispatch. auth_type drives which credential fields render.
export function ClusterForm({
  form,
  setForm,
  projects,
  pending,
  onSave,
  onCancel,
}: {
  form: FormDraft;
  setForm: (f: FormDraft) => void;
  projects: ProjectOption[];
  pending: boolean;
  onSave: () => void;
  onCancel: () => void;
}) {
  const isEdit = form.id !== null;
  const toggleProject = (id: string) => {
    const has = form.allowedProjects.includes(id);
    setForm({
      ...form,
      allowedProjects: has
        ? form.allowedProjects.filter((p) => p !== id)
        : [...form.allowedProjects, id],
    });
  };

  return (
    <div className="grid gap-4 px-6 pb-6">
      <Field label="Name" required htmlFor="cluster-name">
        <Input
          id="cluster-name"
          value={form.name}
          onChange={(e) => setForm({ ...form, name: e.target.value })}
          placeholder="prod-us-east"
          autoFocus
          // Name is the dispatch-time identity of a `cluster:` ref, so
          // it's immutable — delete + recreate to rename.
          disabled={isEdit}
        />
      </Field>
      <Field label="Description" htmlFor="cluster-description">
        <Input
          id="cluster-description"
          value={form.description}
          onChange={(e) => setForm({ ...form, description: e.target.value })}
          placeholder="What this cluster is for"
        />
      </Field>

      <Field
        label="Auth type"
        htmlFor="cluster-auth-type"
        hint="kubeconfig: full kubeconfig · token: API server + CA + bearer token · in-cluster: agent ServiceAccount"
      >
        <Select
          items={AUTH_LABELS}
          value={form.auth_type}
          onValueChange={(v) => {
            if (typeof v === "string") {
              setForm({ ...form, auth_type: v as ClusterAuthType });
            }
          }}
        >
          <SelectTrigger
            id="cluster-auth-type"
            aria-label="Auth type"
            className="w-full"
          >
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {clusterAuthTypes.map((t) => (
              <SelectItem key={t} value={t}>
                {AUTH_LABELS[t]}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </Field>

      {form.auth_type === "kubeconfig" ? (
        <Field
          label="Kubeconfig"
          htmlFor="cluster-kubeconfig"
          hint={
            isEdit
              ? "Leave blank to keep the stored kubeconfig."
              : "Paste the full kubeconfig YAML."
          }
        >
          <Textarea
            id="cluster-kubeconfig"
            value={form.credential}
            onChange={(e) => setForm({ ...form, credential: e.target.value })}
            placeholder={
              isEdit ? "•••••••• (stored)" : "apiVersion: v1\nkind: Config\n..."
            }
            autoComplete="off"
            spellCheck={false}
            rows={8}
            className="font-mono text-xs break-all resize-y max-h-[40vh] overflow-y-auto"
          />
        </Field>
      ) : null}

      {form.auth_type === "token" ? (
        <>
          <Field label="API server" required htmlFor="cluster-api-server">
            <Input
              id="cluster-api-server"
              value={form.api_server}
              onChange={(e) =>
                setForm({ ...form, api_server: e.target.value })
              }
              placeholder="https://10.0.0.1:6443"
              className="font-mono text-xs"
            />
          </Field>
          <Field
            label="CA certificate"
            required
            htmlFor="cluster-ca-cert"
            hint="PEM-encoded cluster CA certificate. Required — token auth never falls back to insecure TLS."
          >
            <Textarea
              id="cluster-ca-cert"
              value={form.ca_cert}
              onChange={(e) => setForm({ ...form, ca_cert: e.target.value })}
              placeholder="-----BEGIN CERTIFICATE-----"
              autoComplete="off"
              spellCheck={false}
              rows={4}
              className="font-mono text-xs break-all resize-y max-h-[30vh] overflow-y-auto"
            />
          </Field>
          <Field
            label="Bearer token"
            htmlFor="cluster-token"
            hint={
              isEdit
                ? "Leave blank to keep the stored token."
                : "ServiceAccount bearer token."
            }
          >
            <Textarea
              id="cluster-token"
              value={form.credential}
              onChange={(e) => setForm({ ...form, credential: e.target.value })}
              placeholder={isEdit ? "•••••••• (stored)" : "eyJhbGci..."}
              autoComplete="off"
              spellCheck={false}
              rows={3}
              className="font-mono text-xs break-all resize-y max-h-[30vh] overflow-y-auto"
            />
          </Field>
        </>
      ) : null}

      {form.auth_type === "in_cluster" ? (
        <p className="rounded-md border border-border bg-muted/40 px-3 py-2 text-xs text-muted-foreground">
          No credential needed — the agent authenticates with its own
          in-cluster ServiceAccount.
        </p>
      ) : null}

      <Field
        label="Allowed projects"
        hint="Projects permitted to target this cluster. None selected = available to all."
      >
        <div className="max-h-48 overflow-y-auto rounded-md border border-border">
          {projects.length === 0 ? (
            <p className="px-3 py-3 text-center text-xs text-muted-foreground">
              No projects yet.
            </p>
          ) : (
            <ul className="divide-y divide-border">
              {projects.map((p) => {
                const checked = form.allowedProjects.includes(p.id);
                return (
                  <li key={p.id}>
                    <label className="flex cursor-pointer items-center gap-2 px-3 py-2 text-sm hover:bg-accent">
                      <input
                        type="checkbox"
                        checked={checked}
                        onChange={() => toggleProject(p.id)}
                        className="size-4 rounded border-input"
                        aria-label={`Allow ${p.name}`}
                      />
                      <span>{p.name}</span>
                      <span className="ml-auto font-mono text-xs text-muted-foreground">
                        {p.slug}
                      </span>
                    </label>
                  </li>
                );
              })}
            </ul>
          )}
        </div>
      </Field>

      {/* Only meaningful once an allow-list exists: an OPEN cluster already permits
          pipeline-declared targets, so a toggle there would imply the opposite. */}
      {form.allowedProjects.length > 0 ? (
        <Field
          label="Pipeline-declared deploy targets"
          hint="This cluster restricts which projects may use it, so declaring a deploy target in a pipeline's YAML is denied by default. Allow it to let the listed projects self-serve — still limited to those projects."
        >
          <label className="flex cursor-pointer items-center gap-2 rounded-md border border-border px-3 py-2 text-sm hover:bg-accent">
            <input
              type="checkbox"
              checked={form.allowDeclarativeTargets}
              onChange={(e) =>
                setForm({ ...form, allowDeclarativeTargets: e.target.checked })
              }
              className="size-4 rounded border-input"
              aria-label="Allow pipeline-declared deploy targets"
            />
            <span>Allow pipelines to declare their own deploy target</span>
          </label>
        </Field>
      ) : null}

      <div className="mt-2 flex items-center justify-end gap-2">
        <Button variant="ghost" onClick={onCancel} disabled={pending}>
          <X className="mr-2 h-4 w-4" /> Cancel
        </Button>
        <Button onClick={onSave} disabled={pending}>
          {pending ? (
            <>
              <Loader2 className="mr-2 h-4 w-4 animate-spin" /> Saving…
            </>
          ) : (
            "Save"
          )}
        </Button>
      </div>
    </div>
  );
}
