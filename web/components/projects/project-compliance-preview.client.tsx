"use client";

import { useState, useTransition } from "react";
import { Loader2, ShieldCheck } from "lucide-react";
import { toast } from "sonner";

import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { previewEffectivePipeline } from "@/server/actions/compliance";
import type {
  ComplianceFramework,
  EffectivePipelinePreview,
} from "@/server/queries/admin";

// Stages and jobs a policy contributes are namespaced with this prefix (repo
// YAML may not use it), which makes them unambiguous to badge as enforced.
const COMPLIANCE_PREFIX = "_compliance_";

function isComplianceEntry(name: string): boolean {
  return name.startsWith(COMPLIANCE_PREFIX);
}

type Props = {
  slug: string;
  frameworks: ComplianceFramework[];
  assignedIDs: string[];
  initial: EffectivePipelinePreview[];
};

export function ProjectCompliancePreview({
  slug,
  frameworks,
  assignedIDs,
  initial,
}: Props) {
  const [whatIf, setWhatIf] = useState(false);
  const [selected, setSelected] = useState<Set<string>>(new Set(assignedIDs));
  const [views, setViews] = useState<EffectivePipelinePreview[]>(initial);
  const [pending, startTransition] = useTransition();

  const runWhatIf = (ids: Set<string>) => {
    startTransition(async () => {
      const res = await previewEffectivePipeline({
        slug,
        framework_ids: [...ids],
      });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      setViews(res.data);
    });
  };

  const toggleWhatIf = (on: boolean) => {
    setWhatIf(on);
    if (on) runWhatIf(selected);
    else setViews(initial); // back to what runs today
  };

  const toggleFramework = (id: string) => {
    const next = new Set(selected);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    setSelected(next);
    runWhatIf(next);
  };

  return (
    <Card>
      <CardHeader>
        <CardTitle>Effective pipeline preview</CardTitle>
        <CardDescription>
          Admin only. The pipeline definition after compliance policies are
          merged — stages and jobs marked <em>enforced</em> come from policy and
          can&apos;t be removed from the repo. A server-managed{" "}
          <code>_compliance</code> pipeline appears when the project ships no CI
          of its own.
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex items-center gap-3">
          <Switch
            id="compliance-whatif"
            checked={whatIf}
            onCheckedChange={toggleWhatIf}
          />
          <Label htmlFor="compliance-whatif" className="cursor-pointer">
            What-if: preview a hypothetical framework set
          </Label>
          {pending ? (
            <Loader2 className="size-4 animate-spin text-muted-foreground" />
          ) : (
            <Badge variant="outline">
              {whatIf ? "hypothetical" : "what runs today"}
            </Badge>
          )}
        </div>

        {whatIf ? (
          frameworks.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No compliance frameworks defined yet.
            </p>
          ) : (
            <div className="flex flex-wrap gap-1.5" aria-label="What-if frameworks">
              {frameworks.map((f) => {
                const on = selected.has(f.id);
                return (
                  <button
                    key={f.id}
                    type="button"
                    onClick={() => toggleFramework(f.id)}
                    aria-pressed={on}
                    aria-label={`Framework ${f.name}`}
                  >
                    <Badge
                      variant={on ? "default" : "outline"}
                      className={cn("cursor-pointer", !on && "hover:bg-accent")}
                    >
                      {f.name}
                    </Badge>
                  </button>
                );
              })}
            </div>
          )
        ) : null}

        {views.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No pipelines to preview
            {whatIf ? " under this framework set." : " for this project yet."}
          </p>
        ) : (
          <div className={cn("space-y-3", pending && "opacity-60")}>
            {views.map((v) => (
              <PipelinePreview key={v.name} view={v} />
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function PipelinePreview({ view }: { view: EffectivePipelinePreview }) {
  const stages = view.effective.stages;
  const jobs = view.effective.jobs;
  const enforcedJobs = jobs.filter((j) => isComplianceEntry(j.name)).length;

  return (
    <div className="space-y-2 rounded-md border p-3">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-medium">{view.name}</span>
        {view.system_managed ? (
          <Badge variant="secondary">server-managed</Badge>
        ) : null}
        {enforcedJobs > 0 ? (
          <span className="inline-flex items-center gap-1 text-xs text-muted-foreground">
            <ShieldCheck className="size-3.5" />
            {enforcedJobs} enforced job{enforcedJobs === 1 ? "" : "s"}
          </span>
        ) : null}
      </div>

      {stages.length > 0 ? (
        <div className="flex flex-wrap gap-1.5">
          {stages.map((s) => (
            <Badge
              key={s}
              variant={isComplianceEntry(s) ? "default" : "outline"}
              title={isComplianceEntry(s) ? "Enforced by compliance policy" : undefined}
            >
              {s}
            </Badge>
          ))}
        </div>
      ) : null}

      {jobs.length > 0 ? (
        <ul className="space-y-1 text-sm">
          {jobs.map((j) => {
            const enforced = isComplianceEntry(j.name);
            return (
              <li key={j.name} className="flex items-center gap-2">
                <span className={cn(enforced && "font-medium")}>{j.name}</span>
                <span className="text-xs text-muted-foreground">({j.stage})</span>
                {enforced ? (
                  <Badge variant="default" className="gap-1">
                    <ShieldCheck className="size-3" />
                    enforced
                  </Badge>
                ) : null}
              </li>
            );
          })}
        </ul>
      ) : (
        <p className="text-xs text-muted-foreground">No jobs.</p>
      )}
    </div>
  );
}
