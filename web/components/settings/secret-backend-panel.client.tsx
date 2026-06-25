"use client";

import { useState, useTransition } from "react";
import {
  AlertTriangle,
  CheckCircle2,
  KeyRound,
  Loader2,
  Save,
  Trash2,
  Wifi,
  XCircle,
} from "lucide-react";
import { toast } from "sonner";

import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { SOURCE_LABELS } from "@/components/secrets/secret-source-fields.client";
import { secretBackendWriteSchema } from "@/lib/validations";
import {
  deleteSecretBackend,
  setSecretBackend,
  testSecretBackend,
} from "@/server/actions/secret-backends";
import type {
  SecretBackend,
  SecretBackendProbeResult,
  SecretBackendSource,
} from "@/types/api";

import { BackendFields } from "./secret-backend-fields";
import { draftFrom, type BackendDraft } from "./secret-backend-draft";

type Props = { backend: SecretBackend };

// chipTone maps a probe status to the inline result chip colours:
// green for ok, amber for the two "almost" states, red for hard error.
function chipTone(status: SecretBackendProbeResult["status"]): string {
  if (status === "ok") {
    return "border-emerald-500/40 bg-emerald-500/10 text-emerald-600 dark:text-emerald-400";
  }
  if (status === "unauthorized" || status === "unreachable") {
    return "border-amber-500/40 bg-amber-500/10 text-amber-600 dark:text-amber-400";
  }
  return "border-destructive/40 bg-destructive/10 text-destructive";
}

// SecretBackendPanel is one self-contained editor for a single backend
// (vault / gcp / aws). It owns its own draft so the three panels on the
// page edit and save independently. Vault is the only backend with a
// write-only credential ("•••• stored" + preserve-on-blank); gcp/aws
// authenticate with ambient ADC/IRSA and send no credentials.
export function SecretBackendPanel({ backend }: Props) {
  const source: SecretBackendSource = backend.source;
  const label = SOURCE_LABELS[source];
  const [origin, setOrigin] = useState(backend.source_origin);
  const [credConfigured, setCredConfigured] = useState(
    (backend.credential_keys ?? []).length > 0,
  );
  const [draft, setDraft] = useState<BackendDraft>(() => draftFrom(backend));
  const [probe, setProbe] = useState<SecretBackendProbeResult | null>(null);
  const [saving, startSaving] = useTransition();
  const [testing, startTesting] = useTransition();
  const [deleting, startDeleting] = useTransition();
  const busy = saving || testing || deleting;

  // A credential counts as "freshly typed" only for Vault and only when
  // the relevant write-only field is non-empty. gcp/aws never have one.
  const typedCredentials = (): Record<string, string> | undefined => {
    if (source !== "vault") return undefined;
    const creds: Record<string, string> = {};
    // Trim like every other field — a secret_id/token pasted from a terminal
    // often carries a trailing newline, which Vault rejects as "invalid
    // secret id". The value never legitimately has surrounding whitespace.
    if (draft.auth === "approle" && draft.secretId.trim()) {
      creds.secret_id = draft.secretId.trim();
    }
    if (draft.auth === "token" && draft.token.trim()) {
      creds.token = draft.token.trim();
    }
    return Object.keys(creds).length > 0 ? creds : undefined;
  };

  function buildValue(): Record<string, unknown> {
    if (source === "vault") {
      const v: Record<string, unknown> = {
        addr: draft.addr.trim(),
        auth: draft.auth,
      };
      if (draft.auth === "approle") v.role_id = draft.roleId.trim();
      if (draft.auth === "kubernetes") {
        v.role = draft.role.trim();
        if (draft.jwtPath.trim()) v.jwt_path = draft.jwtPath.trim();
      }
      if (draft.kvMount.trim()) v.kv_mount = draft.kvMount.trim();
      if (draft.namespace.trim()) v.namespace = draft.namespace.trim();
      if (draft.caCert.trim()) v.ca_cert = draft.caCert.trim();
      // Only send the flag when it's on — keeps the stored value clean and
      // makes "TLS verification off" an explicit, auditable choice.
      if (draft.insecureSkipVerify) v.insecure_skip_verify = true;
      return v;
    }
    if (source === "gcp") {
      return { project: draft.project.trim() };
    }
    const v: Record<string, unknown> = { region: draft.region.trim() };
    if (draft.endpoint.trim()) v.endpoint = draft.endpoint.trim();
    return v;
  }

  // buildInput assembles the action payload. On Vault, a configured
  // credential left blank means "preserve the stored one"; a freshly
  // typed credential ships with preserve_credentials:false.
  function buildInput() {
    const credentials = typedCredentials();
    const input: {
      source: SecretBackendSource;
      enabled: boolean;
      value: Record<string, unknown>;
      credentials?: Record<string, string>;
      preserve_credentials?: boolean;
    } = {
      source,
      enabled: draft.enabled,
      value: buildValue(),
    };
    if (source === "vault") {
      if (credentials) {
        input.credentials = credentials;
        input.preserve_credentials = false;
      } else if (credConfigured && origin === "db") {
        // Preserve only a credential actually stored in the DB. An
        // env-origin credential can't be copied into a DB override, so
        // saving one requires re-typing it (the server enforces this too).
        input.preserve_credentials = true;
      }
    }
    return input;
  }

  // vaultEnvCredRetype: an env-origin Vault credential must be re-typed to
  // save a DB override (it can't be lifted out of the environment).
  const vaultEnvCredRetype =
    source === "vault" &&
    origin === "env" &&
    credConfigured &&
    (draft.auth === "approle" || draft.auth === "token");

  function validateLocally(input: ReturnType<typeof buildInput>): boolean {
    const parsed = secretBackendWriteSchema.safeParse(input);
    if (!parsed.success) {
      toast.error(parsed.error.issues[0]?.message ?? "invalid input");
      return false;
    }
    return true;
  }

  function onSave() {
    const input = buildInput();
    if (!validateLocally(input)) return;
    startSaving(async () => {
      const res = await setSecretBackend(input);
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      setOrigin(res.data.source_origin);
      // credential_keys is a Go []string: a save with no credential (e.g. GCP
      // with project only) marshals it as null, not []. Guard so the client
      // doesn't crash on null/undefined.
      setCredConfigured((res.data.credential_keys ?? []).length > 0);
      setDraft((d) => ({ ...d, secretId: "", token: "" }));
      setProbe(null);
      toast.success(`${label} saved`);
    });
  }

  function onTest() {
    const input = buildInput();
    if (!validateLocally(input)) return;
    startTesting(async () => {
      const res = await testSecretBackend(input);
      if (!res.ok) {
        setProbe({ status: "error", message: res.error });
        return;
      }
      setProbe(res.probe);
    });
  }

  function onDelete() {
    if (
      !window.confirm(
        `Drop the saved ${label} override? The backend falls back to its env config immediately.`,
      )
    ) {
      return;
    }
    startDeleting(async () => {
      const res = await deleteSecretBackend({ source });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      // Resync the form to the post-delete (env-fallback) state so stale
      // override values — e.g. an insecure_skip_verify toggle — can't linger
      // on screen and be re-saved by accident.
      setDraft(draftFrom(res.data));
      setOrigin(res.data.source_origin);
      setCredConfigured((res.data.credential_keys ?? []).length > 0);
      setProbe(null);
      toast.success(`${label} override cleared — using env fallback`);
    });
  }

  return (
    <Card aria-label={label} role="region">
      <CardHeader>
        <div className="flex items-start justify-between gap-3">
          <div>
            <CardTitle className="text-base">{label}</CardTitle>
            <CardDescription>
              {source === "vault"
                ? "Resolve secret refs from HashiCorp Vault KV. Changes apply immediately."
                : source === "gcp"
                  ? "Resolve secret refs from GCP Secret Manager via ambient ADC. Changes apply immediately."
                  : "Resolve secret refs from AWS Secrets Manager via ambient IRSA. Changes apply immediately."}
            </CardDescription>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            {credConfigured ? (
              <Badge variant="outline">
                <KeyRound className="size-3" aria-hidden />
                •••• stored
              </Badge>
            ) : null}
            {origin === "db" ? (
              <Badge variant="success">saved</Badge>
            ) : (
              <Badge variant="outline" className="text-muted-foreground">
                from env
              </Badge>
            )}
          </div>
        </div>
      </CardHeader>
      <CardContent className="space-y-4">
        <label className="flex items-center gap-2 text-sm font-medium">
          <input
            type="checkbox"
            checked={draft.enabled}
            aria-label={`Enable ${label}`}
            onChange={(e) =>
              setDraft((d) => ({ ...d, enabled: e.target.checked }))
            }
          />
          Enabled
        </label>

        {draft.enabled ? (
          <BackendFields
            source={source}
            draft={draft}
            setDraft={setDraft}
            credConfigured={credConfigured}
          />
        ) : null}

        {draft.enabled && vaultEnvCredRetype ? (
          <p className="text-xs text-amber-600 dark:text-amber-400">
            This credential comes from the environment. Re-enter it to save a
            database override — it can&apos;t be copied out of the environment.
          </p>
        ) : null}

        {probe ? (
          <div
            role="status"
            className={cn(
              "flex items-start gap-2 rounded-md border px-3 py-2 text-sm",
              chipTone(probe.status),
            )}
          >
            {probe.status === "ok" ? (
              <CheckCircle2 className="mt-0.5 size-4 shrink-0" aria-hidden />
            ) : probe.status === "error" ? (
              <XCircle className="mt-0.5 size-4 shrink-0" aria-hidden />
            ) : (
              <AlertTriangle className="mt-0.5 size-4 shrink-0" aria-hidden />
            )}
            <span className="shrink-0 font-medium capitalize">{probe.status}</span>
            {probe.message ? (
              <span className="min-w-0 flex-1 break-words text-muted-foreground">
                — {probe.message}
              </span>
            ) : null}
          </div>
        ) : null}

        <div className="flex flex-wrap items-center justify-end gap-2 border-t pt-4">
          {origin === "db" ? (
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={onDelete}
              disabled={busy}
              aria-label={`Delete ${label} override`}
            >
              {deleting ? (
                <Loader2 className="size-4 animate-spin" aria-hidden />
              ) : (
                <Trash2 className="size-4" aria-hidden />
              )}
              Clear override
            </Button>
          ) : null}
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={onTest}
            disabled={busy}
            aria-label={`Test ${label}`}
          >
            {testing ? (
              <Loader2 className="size-4 animate-spin" aria-hidden />
            ) : (
              <Wifi className="size-4" aria-hidden />
            )}
            Test connection
          </Button>
          <Button
            type="button"
            size="sm"
            onClick={onSave}
            disabled={busy}
            aria-label={`Save ${label}`}
          >
            {saving ? (
              <Loader2 className="size-4 animate-spin" aria-hidden />
            ) : (
              <Save className="size-4" aria-hidden />
            )}
            Save
          </Button>
        </div>
      </CardContent>
    </Card>
  );
}
