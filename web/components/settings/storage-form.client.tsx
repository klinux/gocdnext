"use client";

import { useEffect, useMemo, useState, useTransition } from "react";
import {
  AlertTriangle,
  CheckCircle2,
  HardDrive,
  KeyRound,
  Loader2,
  RefreshCw,
  Save,
  Trash2,
} from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  clearStorageConfig,
  saveStorageConfig,
} from "@/server/actions/storage";
import type { StorageConfig } from "@/server/queries/admin";

import { FilesystemPanel } from "./storage-form-fields";
import {
  GCSPanel,
  S3Panel,
  type Draft,
} from "./storage-form-panels";

type Backend = Draft["backend"];

type Props = {
  initial: StorageConfig;
};

function draftFrom(cfg: StorageConfig): Draft {
  const v = cfg.value ?? {};
  return {
    backend: cfg.backend,
    s3Bucket: typeof v.bucket === "string" && cfg.backend === "s3" ? v.bucket : "",
    s3Region: typeof v.region === "string" ? v.region : "",
    s3Endpoint: typeof v.endpoint === "string" ? v.endpoint : "",
    s3UsePathStyle: v.use_path_style === true,
    s3EnsureBucket: v.ensure_bucket === true,
    s3AccessKey: "",
    s3SecretKey: "",
    gcsBucket: typeof v.bucket === "string" && cfg.backend === "gcs" ? v.bucket : "",
    gcsProjectID: typeof v.project_id === "string" ? v.project_id : "",
    gcsEnsureBucket: v.ensure_bucket === true,
    gcsServiceAccountJSON: "",
  };
}

export function StorageForm({ initial }: Props) {
  const [config, setConfig] = useState<StorageConfig>(initial);
  const [draft, setDraft] = useState<Draft>(() => draftFrom(initial));
  const [replaceCreds, setReplaceCreds] = useState(false);
  const [restartRequired, setRestartRequired] = useState(false);
  const [pending, startTransition] = useTransition();
  const [clearing, startClearing] = useTransition();

  // Reset replace toggle whenever the loaded config or backend changes —
  // a fresh "Replace credentials" intent shouldn't survive a backend
  // switch (different field set).
  useEffect(() => {
    setReplaceCreds(false);
  }, [config.source, config.backend, draft.backend]);

  const sourceBadge = useMemo(() => {
    if (config.source === "db") {
      return (
        <span className="inline-flex items-center gap-1 rounded-full border border-emerald-500/40 bg-emerald-500/10 px-2 py-0.5 text-[11px] font-medium text-emerald-600 dark:text-emerald-400">
          <CheckCircle2 className="size-3" aria-hidden />
          DB override
        </span>
      );
    }
    return (
      <span className="inline-flex items-center gap-1 rounded-full border border-border bg-muted/40 px-2 py-0.5 text-[11px] font-medium text-muted-foreground">
        <HardDrive className="size-3" aria-hidden />
        Env fallback
      </span>
    );
  }, [config.source]);

  const credsConfigured = config.credential_keys.length > 0;
  const credsEditable = !credsConfigured || replaceCreds;
  const dbBacked = config.source === "db";

  function setBackend(next: Backend) {
    setDraft((d) => ({ ...d, backend: next }));
  }

  function buildPayload() {
    const backend = draft.backend;
    if (backend === "filesystem") {
      return {
        backend,
        value: {} as Record<string, unknown>,
        credentials: {} as Record<string, string>,
      };
    }
    if (backend === "s3") {
      const credentials: Record<string, string> = {};
      if (credsEditable) {
        if (draft.s3AccessKey.trim()) {
          credentials.access_key = draft.s3AccessKey;
        }
        if (draft.s3SecretKey.trim()) {
          credentials.secret_key = draft.s3SecretKey;
        }
      }
      return {
        backend,
        value: {
          bucket: draft.s3Bucket.trim(),
          region: draft.s3Region.trim(),
          endpoint: draft.s3Endpoint.trim(),
          use_path_style: draft.s3UsePathStyle,
          ensure_bucket: draft.s3EnsureBucket,
        },
        credentials,
      };
    }
    const credentials: Record<string, string> = {};
    if (credsEditable && draft.gcsServiceAccountJSON.trim()) {
      credentials.service_account_json = draft.gcsServiceAccountJSON;
    }
    return {
      backend,
      value: {
        bucket: draft.gcsBucket.trim(),
        project_id: draft.gcsProjectID.trim(),
        ensure_bucket: draft.gcsEnsureBucket,
      },
      credentials,
    };
  }

  function onSave() {
    const payload = buildPayload();
    if (payload.backend === "s3" && !payload.value.bucket) {
      toast.error("S3 bucket is required");
      return;
    }
    if (payload.backend === "gcs" && !payload.value.bucket) {
      toast.error("GCS bucket is required");
      return;
    }
    startTransition(async () => {
      const res = await saveStorageConfig(payload);
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      setConfig({
        backend: payload.backend,
        value: payload.value,
        credential_keys:
          Object.keys(payload.credentials).length > 0
            ? Object.keys(payload.credentials).sort()
            : credsConfigured && !replaceCreds
              ? config.credential_keys
              : [],
        updated_at: new Date().toISOString(),
        source: "db",
      });
      setDraft((d) => ({
        ...d,
        s3AccessKey: "",
        s3SecretKey: "",
        gcsServiceAccountJSON: "",
      }));
      setReplaceCreds(false);
      setRestartRequired(res.data.restart_required);
      toast.success("Storage configuration saved");
    });
  }

  function onClear() {
    if (!dbBacked) return;
    if (
      !window.confirm(
        "Drop the database override? The server falls back to env config on next restart.",
      )
    ) {
      return;
    }
    startClearing(async () => {
      const res = await clearStorageConfig();
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      setConfig((c) => ({
        ...c,
        source: "env",
        credential_keys: [],
        updated_at: undefined,
        updated_by: undefined,
      }));
      setReplaceCreds(false);
      setRestartRequired(res.data.restart_required);
      toast.success("Override cleared — restart to apply env fallback");
    });
  }

  function onResetForm() {
    setDraft(draftFrom(config));
    setReplaceCreds(false);
  }

  return (
    <div className="space-y-4">
      {restartRequired ? (
        <Card className="border-amber-500/40 bg-amber-500/5">
          <CardHeader className="flex-row items-start gap-3 space-y-0">
            <AlertTriangle
              className="mt-0.5 size-5 shrink-0 text-amber-500"
              aria-hidden
            />
            <div>
              <CardTitle className="text-base">Restart required</CardTitle>
              <CardDescription className="mt-1">
                The control-plane reads the storage backend at boot. Restart
                the{" "}
                <code className="rounded bg-muted px-1 font-mono text-xs">
                  gocdnext-server
                </code>{" "}
                pod for the change to take effect. Hot-reload is on the
                roadmap.
              </CardDescription>
            </div>
          </CardHeader>
        </Card>
      ) : null}

      <Card>
        <CardHeader>
          <div className="flex items-center justify-between gap-3">
            <div>
              <CardTitle className="text-base">Active configuration</CardTitle>
              <CardDescription>
                Backend the server is using right now. The DB override wins
                over env when present.
              </CardDescription>
            </div>
            {sourceBadge}
          </div>
        </CardHeader>
        <CardContent className="grid grid-cols-2 gap-4 text-sm md:grid-cols-3">
          <div>
            <div className="text-xs text-muted-foreground">Backend</div>
            <div className="mt-0.5 font-medium capitalize">{config.backend}</div>
          </div>
          <div>
            <div className="text-xs text-muted-foreground">Credentials</div>
            <div className="mt-0.5 font-medium">
              {credsConfigured ? (
                <span className="inline-flex items-center gap-1.5">
                  <KeyRound className="size-3.5" aria-hidden />
                  •••• stored
                </span>
              ) : (
                <span className="text-muted-foreground">none</span>
              )}
            </div>
          </div>
          <div>
            <div className="text-xs text-muted-foreground">Last update</div>
            <div className="mt-0.5 font-medium">
              {config.updated_at ? fmtRelative(config.updated_at) : "—"}
            </div>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Configure backend</CardTitle>
          <CardDescription>
            Pick a backend and fill in the bucket / region / credentials. A
            saved override takes effect on next pod restart and supersedes
            the env values for{" "}
            <code className="rounded bg-muted px-1 font-mono text-xs">
              GOCDNEXT_ARTIFACTS_*
            </code>
            .
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-5">
          <Tabs
            value={draft.backend}
            onValueChange={(v) => setBackend(v as Backend)}
          >
            <TabsList>
              <TabsTrigger value="filesystem">Filesystem</TabsTrigger>
              <TabsTrigger value="s3">S3</TabsTrigger>
              <TabsTrigger value="gcs">GCS</TabsTrigger>
            </TabsList>
          </Tabs>

          {draft.backend === "filesystem" ? <FilesystemPanel /> : null}

          {draft.backend === "s3" ? (
            <S3Panel
              draft={draft}
              setDraft={setDraft}
              credsConfigured={credsConfigured && config.backend === "s3"}
              credsEditable={credsEditable || config.backend !== "s3"}
              replaceCreds={replaceCreds}
              setReplaceCreds={setReplaceCreds}
            />
          ) : null}

          {draft.backend === "gcs" ? (
            <GCSPanel
              draft={draft}
              setDraft={setDraft}
              credsConfigured={credsConfigured && config.backend === "gcs"}
              credsEditable={credsEditable || config.backend !== "gcs"}
              replaceCreds={replaceCreds}
              setReplaceCreds={setReplaceCreds}
            />
          ) : null}

          <div className="flex flex-wrap items-center justify-end gap-2 border-t pt-4">
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={onResetForm}
              disabled={pending || clearing}
            >
              <RefreshCw className="size-4" aria-hidden />
              Reset
            </Button>
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={onClear}
              disabled={!dbBacked || pending || clearing}
            >
              {clearing ? (
                <Loader2 className="size-4 animate-spin" aria-hidden />
              ) : (
                <Trash2 className="size-4" aria-hidden />
              )}
              Clear override
            </Button>
            <Button
              type="button"
              size="sm"
              onClick={onSave}
              disabled={pending || clearing}
            >
              {pending ? (
                <Loader2 className="size-4 animate-spin" aria-hidden />
              ) : (
                <Save className="size-4" aria-hidden />
              )}
              Save configuration
            </Button>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}

function fmtRelative(at: string) {
  try {
    const then = new Date(at).getTime();
    const diff = Date.now() - then;
    const mins = Math.round(diff / 60000);
    if (mins < 1) return "just now";
    if (mins < 60) return `${mins}m ago`;
    const hrs = Math.round(mins / 60);
    if (hrs < 24) return `${hrs}h ago`;
    const days = Math.round(hrs / 24);
    return `${days}d ago`;
  } catch {
    return at;
  }
}
