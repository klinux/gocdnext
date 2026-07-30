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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { setProjectCheckReporting } from "@/server/actions/project-settings";
import type { CheckReportingMode } from "@/server/queries/projects";

const MODES: CheckReportingMode[] = ["both", "check_run", "commit_status"];
// base-ui's Select.Value renders the raw value unless `items` maps it — so the
// trigger shows the friendly label, not "commit_status".
const MODE_LABELS: Record<string, string> = {
  both: "Both — Check Run + Commit Status",
  check_run: "Check Run only",
  commit_status: "Commit Status only",
};

type Props = {
  slug: string;
  initialMode: CheckReportingMode;
};

// hint tailors the helptext to the chosen mode. The check_run case carries the
// branch-protection warning: dropping the commit status can strand a Required
// status check (gocdnext can't read branch protection, so we surface it here).
function hint(mode: CheckReportingMode, slug: string): React.ReactNode {
  switch (mode) {
    case "both":
      return "Posts the rich Check Run (coverage + security + GitHub re-run) and the legacy Commit Status (links straight to the run). Safe default.";
    case "check_run":
      return (
        <>
          Posts only the Check Run. This <strong>stops posting</strong> the
          commit status{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">
            ci/gocdnext/{slug}/&lt;pipeline&gt;
          </code>
          . If a branch-protection rule marks that context as{" "}
          <strong>Required</strong>, PRs will hang until you repoint the rule at{" "}
          <code className="rounded bg-muted px-1 py-0.5 text-xs">
            gocdnext / &lt;pipeline&gt;
          </code>
          .
        </>
      );
    case "commit_status":
      return "Posts only the Commit Status (Woodpecker/GoCD style, links straight to the run). Drops the rich Check Run — no coverage/security summary and no GitHub re-run button.";
  }
}

export function ProjectGithubChecksSettings({ slug, initialMode }: Props) {
  const [mode, setMode] = useState<CheckReportingMode>(initialMode);
  const [pending, startTransition] = useTransition();

  const dirty = mode !== initialMode;

  const onSave = () => {
    startTransition(async () => {
      const res = await setProjectCheckReporting({ slug, mode });
      if (res.ok) {
        toast.success(`GitHub checks: ${MODE_LABELS[mode]}`);
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
          <CardTitle className="text-base">GitHub checks</CardTitle>
        </div>
        <CardDescription>
          How gocdnext reports each run back to GitHub — as a rich Check Run
          (&nbsp;
          <code className="rounded bg-muted px-1 py-0.5 text-xs">
            gocdnext / &lt;pipeline&gt;
          </code>
          ), a legacy Commit Status, or both.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <Select
          items={MODE_LABELS}
          value={mode}
          disabled={pending}
          onValueChange={(v) => {
            if (typeof v === "string") setMode(v as CheckReportingMode);
          }}
        >
          <SelectTrigger
            aria-label="GitHub check reporting mode"
            className="w-72"
          >
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {MODES.map((m) => (
              <SelectItem key={m} value={m}>
                {MODE_LABELS[m]}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>

        <p
          className={
            mode === "check_run"
              ? "text-xs text-amber-600 dark:text-amber-500"
              : "text-xs text-muted-foreground"
          }
        >
          {hint(mode, slug)}
        </p>

        <Button onClick={onSave} disabled={!dirty || pending}>
          {pending ? (
            <Loader2 className="mr-2 size-4 animate-spin" aria-hidden />
          ) : null}
          Save
        </Button>
      </CardContent>
    </Card>
  );
}
