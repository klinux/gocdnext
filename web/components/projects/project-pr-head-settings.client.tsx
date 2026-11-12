"use client";

import { useState, useTransition } from "react";
import { GitPullRequestArrow, Loader2 } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Switch } from "@/components/ui/switch";
import { setProjectPrHeadTrust } from "@/server/actions/project-settings";

type Props = {
  slug: string;
  initialEnabled: boolean;
  // Writing the toggle is admin-only on the server; mirror that in the UI so a
  // non-admin sees the current state read-only instead of a 403 on save.
  canEdit: boolean;
};

export function ProjectPrHeadSettings({
  slug,
  initialEnabled,
  canEdit,
}: Props) {
  const [enabled, setEnabled] = useState(initialEnabled);
  const [pending, startTransition] = useTransition();

  const dirty = enabled !== initialEnabled;

  const onSave = () => {
    startTransition(async () => {
      const res = await setProjectPrHeadTrust({ slug, enabled });
      if (res.ok) {
        toast.success(
          enabled
            ? "Same-repo PRs now run their own config"
            : "Same-repo PRs run the default branch config",
        );
      } else {
        toast.error(res.error);
      }
    });
  };

  return (
    <Card>
      <CardHeader>
        <div className="flex items-center gap-2">
          <GitPullRequestArrow
            className="size-4 text-muted-foreground"
            aria-hidden
          />
          <CardTitle className="text-base">Pull request config</CardTitle>
        </div>
        <CardDescription>
          When on, a same-repo (non-fork) pull request runs against its own{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">.gocdnext/</code>{" "}
          at the PR head, so a PR that changes a pipeline validates itself
          instead of running the default branch&apos;s config. The run is
          ephemeral — it never overwrites the project&apos;s saved definition.
          Because the PR author then controls the jobs, images, agent profiles,
          OIDC and deploys that run with this project&apos;s secrets, enabling is
          admin-only and audited. Fork PRs never use head config.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex items-center gap-3">
          <Switch
            id="pr-head-trust"
            checked={enabled}
            disabled={!canEdit || pending}
            onCheckedChange={(v) => setEnabled(Boolean(v))}
            aria-label="Run same-repo PRs against their own config"
          />
          <label htmlFor="pr-head-trust" className="text-sm">
            Run same-repo PRs against their own config
          </label>
        </div>

        {canEdit ? null : (
          <p className="text-xs text-muted-foreground">
            Changing this requires an admin.
          </p>
        )}

        <Button onClick={onSave} disabled={!dirty || !canEdit || pending}>
          {pending ? (
            <Loader2 className="mr-2 size-4 animate-spin" aria-hidden />
          ) : null}
          Save
        </Button>
      </CardContent>
    </Card>
  );
}
