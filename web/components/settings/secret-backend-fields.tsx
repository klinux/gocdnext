import type React from "react";

import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import type { SecretBackendSource } from "@/types/api";

import { Field } from "./storage-form-fields";
import type { BackendDraft } from "./secret-backend-draft";

// Presentational — not "use client". Renders over a BackendDraft owned
// by the parent panel; setDraft is passed straight through so each
// control updates one field without lifting every onChange.
type Props = {
  source: SecretBackendSource;
  draft: BackendDraft;
  setDraft: React.Dispatch<React.SetStateAction<BackendDraft>>;
  credConfigured: boolean;
};

const VAULT_AUTHS: { value: BackendDraft["auth"]; label: string }[] = [
  { value: "approle", label: "AppRole" },
  { value: "kubernetes", label: "Kubernetes" },
  { value: "token", label: "Token" },
];

// Label lookup for the Select trigger — base-ui renders the raw value
// unless `items` maps it to the human label.
const VAULT_AUTH_LABELS: Record<string, string> = Object.fromEntries(
  VAULT_AUTHS.map((a) => [a.value, a.label]),
);

// credentialHint reflects whether a Vault credential is already stored.
function credentialHint(configured: boolean): string {
  return configured
    ? "A credential is stored. Leave blank to keep it; type a new value to replace it."
    : "Write-only. Stored encrypted at rest and never echoed back.";
}

export function BackendFields({ source, draft, setDraft, credConfigured }: Props) {
  // Field ids are source-prefixed so the three panels never collide.
  const id = (k: string) => `sb-${source}-${k}`;

  if (source === "gcp") {
    return (
      <Field
        label="GCP project"
        required
        htmlFor={id("project")}
        hint="Project that owns the secrets. Authentication uses ambient ADC / Workload Identity."
      >
        <Input
          id={id("project")}
          value={draft.project}
          onValueChange={(v) => setDraft((d) => ({ ...d, project: v }))}
          placeholder="my-project"
          autoComplete="off"
        />
      </Field>
    );
  }

  if (source === "aws") {
    return (
      <div className="grid gap-4 md:grid-cols-2">
        <Field
          label="AWS region"
          required
          htmlFor={id("region")}
          hint="Region of the secrets. Auth uses ambient IRSA / instance role."
        >
          <Input
            id={id("region")}
            value={draft.region}
            onValueChange={(v) => setDraft((d) => ({ ...d, region: v }))}
            placeholder="us-east-1"
            autoComplete="off"
          />
        </Field>
        <Field
          label="AWS endpoint"
          htmlFor={id("endpoint")}
          hint="Leave empty for AWS. Set for LocalStack / a VPC endpoint."
        >
          <Input
            id={id("endpoint")}
            value={draft.endpoint}
            onValueChange={(v) => setDraft((d) => ({ ...d, endpoint: v }))}
            placeholder="https://secretsmanager.example.com"
            autoComplete="off"
          />
        </Field>
      </div>
    );
  }

  // Vault.
  return (
    <div className="space-y-4">
      <div className="grid gap-4 md:grid-cols-2">
        <Field label="Vault address" required htmlFor={id("addr")}>
          <Input
            id={id("addr")}
            value={draft.addr}
            onValueChange={(v) => setDraft((d) => ({ ...d, addr: v }))}
            placeholder="https://vault.example.com"
            autoComplete="off"
            className="font-mono text-xs"
          />
        </Field>
        <Field label="Auth method" htmlFor={id("auth")}>
          <Select
            items={VAULT_AUTH_LABELS}
            value={draft.auth}
            onValueChange={(v) => {
              if (typeof v === "string") {
                setDraft((d) => ({ ...d, auth: v as BackendDraft["auth"] }));
              }
            }}
          >
            <SelectTrigger
              id={id("auth")}
              aria-label="Auth method"
              className="w-full"
            >
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {VAULT_AUTHS.map((a) => (
                <SelectItem key={a.value} value={a.value}>
                  {a.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>
      </div>

      {draft.auth === "approle" ? (
        <div className="grid gap-4 md:grid-cols-2">
          <Field
            label="Role ID"
            required
            htmlFor={id("role_id")}
            hint="AppRole role identifier (non-secret)."
          >
            <Input
              id={id("role_id")}
              value={draft.roleId}
              onValueChange={(v) => setDraft((d) => ({ ...d, roleId: v }))}
              placeholder="00000000-0000-0000-0000-000000000000"
              autoComplete="off"
              className="font-mono text-xs"
            />
          </Field>
          <Field
            label="AppRole secret_id"
            htmlFor={id("secret_id")}
            hint={credentialHint(credConfigured)}
          >
            <Input
              id={id("secret_id")}
              type="password"
              value={draft.secretId}
              onValueChange={(v) => setDraft((d) => ({ ...d, secretId: v }))}
              placeholder={credConfigured ? "•••• stored" : "secret-id"}
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
        </div>
      ) : null}

      {draft.auth === "kubernetes" ? (
        <div className="grid gap-4 md:grid-cols-2">
          <Field
            label="Vault role"
            required
            htmlFor={id("role")}
            hint="Vault Kubernetes-auth role to assume."
          >
            <Input
              id={id("role")}
              value={draft.role}
              onValueChange={(v) => setDraft((d) => ({ ...d, role: v }))}
              placeholder="ci"
              autoComplete="off"
            />
          </Field>
          <Field
            label="JWT path"
            htmlFor={id("jwt_path")}
            hint="ServiceAccount token path. Defaults to the standard projected path."
          >
            <Input
              id={id("jwt_path")}
              value={draft.jwtPath}
              onValueChange={(v) => setDraft((d) => ({ ...d, jwtPath: v }))}
              placeholder="/var/run/secrets/kubernetes.io/serviceaccount/token"
              autoComplete="off"
              className="font-mono text-xs"
            />
          </Field>
        </div>
      ) : null}

      {draft.auth === "token" ? (
        <Field label="Token" htmlFor={id("token")} hint={credentialHint(credConfigured)}>
          <Input
            id={id("token")}
            type="password"
            value={draft.token}
            onValueChange={(v) => setDraft((d) => ({ ...d, token: v }))}
            placeholder={credConfigured ? "•••• stored" : "hvs...."}
            autoComplete="off"
            spellCheck={false}
            className="font-mono text-xs"
          />
        </Field>
      ) : null}

      <div className="grid gap-4 md:grid-cols-2">
        <Field
          label="KV mount"
          htmlFor={id("kv_mount")}
          hint="Optional. Defaults to the server's configured mount."
        >
          <Input
            id={id("kv_mount")}
            value={draft.kvMount}
            onValueChange={(v) => setDraft((d) => ({ ...d, kvMount: v }))}
            placeholder="secret"
            autoComplete="off"
          />
        </Field>
        <Field
          label="Namespace"
          htmlFor={id("namespace")}
          hint="Optional. Vault Enterprise namespace."
        >
          <Input
            id={id("namespace")}
            value={draft.namespace}
            onValueChange={(v) => setDraft((d) => ({ ...d, namespace: v }))}
            placeholder="admin/ci"
            autoComplete="off"
          />
        </Field>
      </div>

      <Field
        label="CA certificate (PEM)"
        htmlFor={id("ca_cert")}
        hint="Optional. PEM bundle to verify a private/internal Vault CA. The proper fix for an 'unknown authority' TLS error — prefer this over skipping verification."
      >
        <Textarea
          id={id("ca_cert")}
          value={draft.caCert}
          onChange={(e) => setDraft((d) => ({ ...d, caCert: e.target.value }))}
          placeholder={"-----BEGIN CERTIFICATE-----\n..."}
          className="h-28 font-mono text-xs"
          spellCheck={false}
        />
      </Field>

      <div className="flex items-start gap-3">
        <Switch
          id={id("insecure_skip_verify")}
          checked={draft.insecureSkipVerify}
          onCheckedChange={(v) =>
            setDraft((d) => ({ ...d, insecureSkipVerify: v === true }))
          }
        />
        <div className="space-y-0.5">
          <Label htmlFor={id("insecure_skip_verify")} className="cursor-pointer">
            Skip TLS verification
          </Label>
          <p className="text-xs text-muted-foreground">
            Disables certificate validation for the Vault connection. Use only
            for an internal Vault you control — prefer a CA certificate above.
            The server logs a warning while this is on.
          </p>
        </div>
      </div>
    </div>
  );
}
