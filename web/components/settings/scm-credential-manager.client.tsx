"use client";

import { useState, useTransition } from "react";
import { Loader2, Plus, Save, Trash2, X } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  upsertSCMCredential,
  deleteSCMCredential,
} from "@/server/actions/scm-credentials";
import type { SCMCredential } from "@/types/api";

type Props = {
  provider: "gitlab" | "bitbucket";
  credentials: SCMCredential[];
  defaultHost: string;
  apiBasePlaceholder: string;
  authHint: string;
};

type DraftForm = {
  host: string;
  api_base: string;
  display_name: string;
  auth_ref: string;
};

function blankDraft(defaultHost: string): DraftForm {
  return { host: defaultHost, api_base: "", display_name: "", auth_ref: "" };
}

// SCMCredentialManager renders the org-level credential list for
// one provider + a compact inline form to add / rotate. No edit
// flow (beyond rotation via re-add) — the unique (provider, host)
// key makes upsert-by-repost the natural path, and rotation is
// the only field that changes often enough to warrant a button.
export function SCMCredentialManager({
  provider,
  credentials,
  defaultHost,
  apiBasePlaceholder,
  authHint,
}: Props) {
  const [list, setList] = useState<SCMCredential[]>(credentials);
  const [draft, setDraft] = useState<DraftForm | null>(null);
  const [pending, startTransition] = useTransition();

  const save = () => {
    if (!draft) return;
    const host = draft.host.trim().toLowerCase();
    const authRef = draft.auth_ref.trim();
    if (!host) {
      toast.error("Host is required");
      return;
    }
    if (!authRef) {
      toast.error("Token is required");
      return;
    }
    startTransition(async () => {
      const res = await upsertSCMCredential({
        provider,
        host,
        api_base: draft.api_base.trim() || undefined,
        display_name: draft.display_name.trim() || undefined,
        auth_ref: authRef,
      });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      const optimistic: SCMCredential = {
        id: "__opt__" + Date.now(),
        provider,
        host,
        api_base: draft.api_base.trim() || undefined,
        display_name: draft.display_name.trim() || undefined,
        auth_ref_preview:
          authRef.length > 8
            ? authRef.slice(0, 4) + "…" + authRef.slice(-4)
            : "•".repeat(authRef.length),
        created_at: new Date().toISOString(),
        updated_at: new Date().toISOString(),
      };
      setList((prev) => {
        const without = prev.filter((c) => c.host !== host);
        return [...without, optimistic].sort((a, b) =>
          a.host.localeCompare(b.host),
        );
      });
      setDraft(null);
      toast.success("Credential saved");
    });
  };

  const remove = (c: SCMCredential) => {
    if (!confirm(`Remove credential for ${c.host}? Bound projects will fall back to their per-project PAT, if any.`)) {
      return;
    }
    startTransition(async () => {
      const res = await deleteSCMCredential({ id: c.id });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      setList((prev) => prev.filter((x) => x.id !== c.id));
      toast.success("Credential removed");
    });
  };

  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between">
        <p className="text-xs font-medium text-muted-foreground">
          Org-level credentials
        </p>
        {!draft ? (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setDraft(blankDraft(defaultHost))}
            disabled={pending}
          >
            <Plus className="mr-1 size-3.5" />
            Add
          </Button>
        ) : null}
      </div>

      {list.length === 0 && !draft ? (
        <p className="text-xs text-muted-foreground">
          None yet. Without an org credential, bind-time falls back to the
          per-project <code className="rounded bg-muted px-1">auth_ref</code>.
        </p>
      ) : null}

      {list.length > 0 ? (
        <ul className="space-y-1.5">
          {list.map((c) => (
            <li
              key={c.id}
              className="flex items-center justify-between gap-2 rounded-md border bg-background px-2.5 py-1.5 text-xs"
            >
              <div className="min-w-0 flex-1">
                <div className="font-medium">{c.host}</div>
                <div className="mt-0.5 text-muted-foreground">
                  {c.auth_ref_preview ? (
                    <span className="font-mono">{c.auth_ref_preview}</span>
                  ) : null}
                  {c.api_base ? (
                    <span className="ml-2">api: {c.api_base}</span>
                  ) : null}
                </div>
              </div>
              <Button
                variant="ghost"
                size="sm"
                onClick={() => remove(c)}
                disabled={pending}
                aria-label={`Remove ${c.host}`}
              >
                <Trash2 className="size-3.5" />
              </Button>
            </li>
          ))}
        </ul>
      ) : null}

      {draft ? (
        <div className="space-y-2 rounded-md border bg-background p-3">
          <div className="flex items-center justify-between">
            <p className="text-xs font-medium">New credential</p>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => setDraft(null)}
              disabled={pending}
            >
              <X className="size-3.5" />
            </Button>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor={`cred-host-${provider}`} className="text-xs">
              Host
            </Label>
            <Input
              id={`cred-host-${provider}`}
              value={draft.host}
              onValueChange={(v) =>
                setDraft((d) => (d ? { ...d, host: v } : d))
              }
              placeholder={defaultHost}
              disabled={pending}
              className="h-8 text-sm"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor={`cred-auth-${provider}`} className="text-xs">
              {authHint}
            </Label>
            <Input
              id={`cred-auth-${provider}`}
              type="password"
              value={draft.auth_ref}
              onValueChange={(v) =>
                setDraft((d) => (d ? { ...d, auth_ref: v } : d))
              }
              disabled={pending}
              className="h-8 text-sm"
            />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor={`cred-apibase-${provider}`} className="text-xs">
              API base (optional — self-hosted only)
            </Label>
            <Input
              id={`cred-apibase-${provider}`}
              value={draft.api_base}
              onValueChange={(v) =>
                setDraft((d) => (d ? { ...d, api_base: v } : d))
              }
              placeholder={apiBasePlaceholder}
              disabled={pending}
              className="h-8 text-sm"
            />
          </div>
          <Button onClick={save} disabled={pending} size="sm">
            {pending ? (
              <Loader2 className="mr-1 size-3.5 animate-spin" />
            ) : (
              <Save className="mr-1 size-3.5" />
            )}
            Save
          </Button>
        </div>
      ) : null}
    </div>
  );
}
