"use client";

import { useMemo, useState, useTransition } from "react";
import { GitMerge, Loader2, ShieldAlert } from "lucide-react";
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
import { Switch } from "@/components/ui/switch";
import { setProjectRequiredChecks } from "@/server/actions/project-settings";
import type { RequiredChecksSettings } from "@/server/queries/projects";

type Props = {
  slug: string;
  initial: RequiredChecksSettings;
};

const SYNC_LABEL: Record<string, string> = {
  synced: "Synced",
  failed: "Sync failed",
  skipped: "Not synced",
  pending: "Pending",
  not_configured: "Not configured",
};

function syncVariant(
  status: string,
): "default" | "secondary" | "destructive" | "outline" {
  if (status === "synced") return "default";
  if (status === "failed") return "destructive";
  return "secondary";
}

export function ProjectRequiredChecks({ slug, initial }: Props) {
  const initialSet = useMemo(
    () => new Set(initial.pipelines),
    [initial.pipelines],
  );
  const [selected, setSelected] = useState<Set<string>>(
    () => new Set(initial.pipelines),
  );
  const [pending, startTransition] = useTransition();

  const dirty = useMemo(() => {
    if (selected.size !== initialSet.size) return true;
    for (const p of selected) if (!initialSet.has(p)) return true;
    return false;
  }, [selected, initialSet]);

  const toggle = (name: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(name)) next.delete(name);
      else next.add(name);
      return next;
    });
  };

  const onSave = () => {
    startTransition(async () => {
      const res = await setProjectRequiredChecks({
        slug,
        pipelines: [...selected],
      });
      if (res.ok) {
        toast.success("Required checks saved");
      } else {
        toast.error(res.error);
      }
    });
  };

  const sync = initial.sync;
  const failedAdmin = sync.status === "failed" && sync.needs_admin;
  // A failed sync is retryable even with no selection change — the operator may
  // have just granted the App permission and wants to re-push the same config.
  const canSubmit =
    (dirty || sync.status === "failed") &&
    initial.available_pipelines.length > 0;

  return (
    <Card>
      <CardHeader>
        <div className="flex items-center justify-between gap-2">
          <div className="flex items-center gap-2">
            <GitMerge className="size-4 text-muted-foreground" aria-hidden />
            <CardTitle className="text-base">Required checks for merge</CardTitle>
          </div>
          {sync.status !== "not_configured" ? (
            <Badge variant={syncVariant(sync.status)}>
              {SYNC_LABEL[sync.status] ?? sync.status}
            </Badge>
          ) : null}
        </div>
        <CardDescription>
          Pick the pipelines that must be green to merge a PR. gocdnext writes a
          dedicated{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">
            gocdnext-required-checks-{slug}
          </code>{" "}
          ruleset on the repo requiring their checks — GitHub blocks the merge
          until they pass.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {failedAdmin ? (
          <div className="flex items-start gap-2 rounded-md border border-amber-300 bg-amber-50 p-3 text-xs text-amber-700 dark:border-amber-800/60 dark:bg-amber-950/40 dark:text-amber-400">
            <ShieldAlert className="mt-0.5 size-4 shrink-0" aria-hidden />
            <span>
              gocdnext couldn&apos;t write the ruleset — the GitHub App is likely
              missing the <strong>Administration: write</strong> permission. Ask
              an org admin to re-approve the App, then <strong>Save</strong> to
              retry.
            </span>
          </div>
        ) : sync.status === "failed" && sync.error ? (
          <p className="text-xs text-destructive">{sync.error}</p>
        ) : null}

        {initial.available_pipelines.length === 0 ? (
          <p className="text-xs text-muted-foreground">
            No pull-request pipelines in this project. A pipeline must run on{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-xs">
              pull_request
            </code>{" "}
            to be required for merge.
          </p>
        ) : (
          <ul className="divide-y rounded-md border">
            {initial.available_pipelines.map((name) => (
              <li
                key={name}
                className="flex items-center justify-between gap-3 px-3 py-2"
              >
                <div className="min-w-0">
                  <div className="truncate text-sm font-medium">{name}</div>
                  <code className="text-xs text-muted-foreground">
                    ci/gocdnext/{slug}/{name}
                  </code>
                </div>
                <Switch
                  checked={selected.has(name)}
                  disabled={pending}
                  onCheckedChange={() => toggle(name)}
                  aria-label={`Require ${name} to merge`}
                />
              </li>
            ))}
          </ul>
        )}

        <Button onClick={onSave} disabled={!canSubmit || pending}>
          {pending ? (
            <Loader2 className="mr-2 size-4 animate-spin" aria-hidden />
          ) : null}
          {failedAdmin ? "Retry" : "Save"}
        </Button>
      </CardContent>
    </Card>
  );
}
