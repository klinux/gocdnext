"use client";

import { useMemo } from "react";
import { Box, Clock, Layers, Loader2 } from "lucide-react";

import { cn } from "@/lib/utils";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { EffectivePipelinePreview } from "@/server/queries/admin";
import type { PolicyDraft } from "./policy-form.client";
import type { PolicyPreviewState, PreviewProject } from "./use-policy-preview";

export function PolicyPreview({
  draft,
  projects,
  frameworkNames,
  preview,
}: {
  draft: PolicyDraft;
  projects: PreviewProject[];
  frameworkNames: string[];
  preview: PolicyPreviewState;
}) {
  const { slug, setSlug, views, loading, error, baseStages } = preview;
  const hasConfig = draft.configYaml.trim().length > 0;
  const projectName = projects.find((p) => p.slug === slug)?.name ?? slug;
  const stats = useMemo(() => aggregate(views), [views]);

  const scopeWord = draft.appliesToAll
    ? "every project in the org"
    : frameworkNames.length === 0
      ? "no projects yet — pick a framework"
      : `projects carrying ${frameworkNames.join(" or ")}`;
  const posWord = placementWord(baseStages, draft.positionBefore, draft.positionAfter);

  return (
    <div className="flex h-full flex-col gap-4">
      <div className="flex items-center gap-2 pr-7 font-mono text-[10.5px] font-semibold uppercase tracking-wider text-muted-foreground">
        Merge preview
        <span className="ml-auto flex items-center gap-1.5 text-[10.5px] normal-case tracking-normal text-primary">
          {loading && hasConfig && slug ? (
            <Loader2 className="size-3 animate-spin" />
          ) : (
            <span className="size-1.5 animate-pulse rounded-full bg-primary" />
          )}
          live
        </span>
      </div>

      {projects.length > 1 ? (
        <Select value={slug} onValueChange={(v) => setSlug(v ?? "")}>
          <SelectTrigger className="w-full">
            <SelectValue placeholder="Preview against…" />
          </SelectTrigger>
          <SelectContent>
            {projects.map((p) => (
              <SelectItem key={p.slug} value={p.slug}>
                <Box className="size-3.5 text-muted-foreground" />
                {p.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      ) : null}

      <p className="text-[12px] leading-snug text-muted-foreground">
        How <b className="font-semibold text-foreground">{draft.name || "this policy"}</b>{" "}
        reshapes <b className="font-semibold text-foreground">{projectName}</b>&apos;s pipeline.
      </p>

      <PreviewBody
        hasConfig={hasConfig}
        hasProject={!!slug}
        loading={loading}
        error={error}
        views={views}
        mode={draft.mode}
        tag={draft.appliesToAll ? "all projects" : (frameworkNames[0] ?? "—")}
      />

      {views && views.length > 0 ? (
        <>
          <div className="grid grid-cols-2 gap-2.5">
            <Stat
              icon={<Layers className="size-3" />}
              label="Added per run"
              value={`+${stats.stages}`}
              sub={`${stats.stages === 1 ? "stage" : "stages"} · +${stats.jobs} ${stats.jobs === 1 ? "job" : "jobs"}`}
              accent
            />
            <Stat
              icon={<Clock className="size-3" />}
              label="Pipelines"
              value={String(stats.pipelines)}
              sub={stats.pipelines === 1 ? "touched" : "touched"}
            />
          </div>

          <div className="flex items-center gap-3 rounded-xl border border-[#a779e9]/30 bg-[#a779e9]/10 px-3.5 py-3">
            <span className="flex size-7 shrink-0 items-center justify-center rounded-lg bg-[#a779e9]/20 text-[#a779e9]">
              <Layers className="size-3.5" />
            </span>
            <p className="text-[12px] leading-snug">
              Governs {scopeWord}. Inserted {posWord}, priority{" "}
              <b className="text-[#a779e9]">{draft.priority}</b>.
            </p>
          </div>
        </>
      ) : null}
    </div>
  );
}

type Stats = { stages: number; jobs: number; pipelines: number };

function aggregate(views: EffectivePipelinePreview[] | null): Stats {
  const out: Stats = { stages: 0, jobs: 0, pipelines: 0 };
  if (!views) return out;
  for (const v of views) {
    const rawStages = new Set(v.raw.stages);
    const rawJobs = new Set(v.raw.jobs.map((j) => j.name));
    const injStages = v.effective.stages.filter((s) => !rawStages.has(s)).length;
    const injJobs = v.effective.jobs.filter((j) => !rawJobs.has(j.name)).length;
    out.stages += injStages;
    out.jobs += injJobs;
    if (injStages > 0 || injJobs > 0) out.pipelines += 1;
  }
  return out;
}

function placementWord(stages: string[], before: string, after: string): string {
  if (before) return `before ${before}`;
  if (after) return `after ${after}`;
  if (stages.length) return `after ${stages[stages.length - 1]}`;
  return "into the pipeline";
}

function Stat({
  icon,
  label,
  value,
  sub,
  accent,
}: {
  icon: React.ReactNode;
  label: string;
  value: string;
  sub: string;
  accent?: boolean;
}) {
  return (
    <div className="rounded-xl border border-border bg-card p-3.5">
      <div className="mb-1.5 flex items-center gap-1.5 font-mono text-[9.5px] uppercase tracking-wide text-muted-foreground">
        {icon}
        {label}
      </div>
      <div className={cn("text-[20px] font-bold tracking-tight", accent && "text-primary")}>
        {value}
      </div>
      <div className="mt-0.5 font-mono text-[11px] text-muted-foreground">{sub}</div>
    </div>
  );
}

function PreviewBody({
  hasConfig,
  hasProject,
  loading,
  error,
  views,
  mode,
  tag,
}: {
  hasConfig: boolean;
  hasProject: boolean;
  loading: boolean;
  error: string | null;
  views: EffectivePipelinePreview[] | null;
  mode: "inject" | "override";
  tag: string;
}) {
  if (!hasProject) return <Hint>No projects to preview against yet.</Hint>;
  if (!hasConfig) return <Hint>Write the policy config to see where its stage lands.</Hint>;
  if (error)
    return (
      <div className="rounded-xl border border-amber-500/40 bg-amber-500/5 p-3 text-xs text-amber-600 dark:text-amber-400">
        {error}
      </div>
    );
  if (!views && loading) return <Hint>Computing merge…</Hint>;
  if (!views || views.length === 0)
    return <Hint>This project has no pipeline to merge into.</Hint>;
  return (
    <div className="space-y-3">
      {views.map((v) => (
        <PipelineCard key={v.name} view={v} mode={mode} tag={tag} />
      ))}
    </div>
  );
}

function Hint({ children }: { children: React.ReactNode }) {
  return (
    <div className="rounded-xl border border-dashed border-border p-4 text-center text-xs text-muted-foreground">
      {children}
    </div>
  );
}

function PipelineCard({
  view,
  mode,
  tag,
}: {
  view: EffectivePipelinePreview;
  mode: "inject" | "override";
  tag: string;
}) {
  const rawStages = useMemo(() => new Set(view.raw.stages), [view.raw.stages]);
  const jobsByStage = useMemo(() => {
    const m = new Map<string, string[]>();
    for (const j of view.effective.jobs) {
      const arr = m.get(j.stage) ?? [];
      arr.push(j.name);
      m.set(j.stage, arr);
    }
    return m;
  }, [view.effective.jobs]);

  return (
    <div className="rounded-2xl border border-border bg-card p-4">
      <div className="mb-3 flex items-center gap-2">
        <span className="font-mono text-xs font-semibold">{view.name}</span>
        <span className="ml-auto rounded border border-border bg-muted px-1.5 py-0.5 font-mono text-[9.5px] text-muted-foreground">
          {tag}
        </span>
      </div>
      <ol className="flex flex-col">
        {view.effective.stages.map((stage, i) => {
          const injected = !rawStages.has(stage);
          const jobs = jobsByStage.get(stage) ?? [];
          return (
            <li key={stage}>
              <div
                className={cn(
                  "flex items-center gap-3",
                  injected && "-mx-2 rounded-lg bg-gradient-to-r from-primary/10 to-transparent px-2",
                )}
              >
                <span
                  className={cn(
                    "size-[13px] shrink-0 rounded-full border-2",
                    injected
                      ? "border-primary bg-primary shadow-[0_0_0_4px] shadow-primary/15"
                      : "border-primary/70 bg-background",
                  )}
                />
                <div className="min-w-0 py-2">
                  <div
                    className={cn(
                      "flex items-center gap-2 font-mono text-[12.5px] font-semibold",
                      injected && "text-primary",
                    )}
                  >
                    {stage}
                    {injected ? (
                      <span className="rounded-full bg-primary px-1.5 py-px font-mono text-[8.5px] font-bold uppercase tracking-wide text-[#06222a]">
                        {mode}
                      </span>
                    ) : null}
                  </div>
                  {jobs.length ? (
                    <div className="truncate font-mono text-[10.5px] text-muted-foreground">
                      {jobs.join(" · ")}
                    </div>
                  ) : null}
                </div>
              </div>
              {i < view.effective.stages.length - 1 ? (
                <span className="ml-[5px] block h-4 w-0.5 bg-primary/35" aria-hidden />
              ) : null}
            </li>
          );
        })}
      </ol>
    </div>
  );
}
