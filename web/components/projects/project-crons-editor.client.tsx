"use client";

import { useState, useTransition } from "react";
import { Clock, Loader2, Pencil, Plus, Save, Trash2, X } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  createProjectCron,
  deleteProjectCron,
  updateProjectCron,
} from "@/server/actions/project-crons";
import type { ProjectCron } from "@/server/queries/projects";

type PipelineOption = { id: string; name: string };

type Props = {
  slug: string;
  initial: ProjectCron[];
  pipelines: PipelineOption[];
};

type DraftForm = {
  id: string | null; // null = creating
  name: string;
  expression: string;
  fireAll: boolean;
  pipelineIds: Set<string>;
  enabled: boolean;
};

function blankDraft(): DraftForm {
  return {
    id: null,
    name: "",
    expression: "",
    fireAll: true,
    pipelineIds: new Set(),
    enabled: true,
  };
}

function draftFromCron(
  cron: ProjectCron,
  allPipelines: PipelineOption[],
): DraftForm {
  const total = allPipelines.length;
  const fireAll = cron.pipeline_ids.length === 0;
  // Pre-select all pipelines when fireAll, so switching fireAll→off
  // inside the form doesn't clear every checkbox.
  return {
    id: cron.id,
    name: cron.name,
    expression: cron.expression,
    fireAll,
    pipelineIds: new Set(fireAll ? allPipelines.map((p) => p.id) : cron.pipeline_ids),
    enabled: cron.enabled,
  };
}

function describeTargets(cron: ProjectCron, pipelines: PipelineOption[]): string {
  if (cron.pipeline_ids.length === 0) return "all pipelines";
  if (cron.pipeline_ids.length === 1) {
    const match = pipelines.find((p) => p.id === cron.pipeline_ids[0]);
    return match ? match.name : "1 pipeline";
  }
  return `${cron.pipeline_ids.length} pipelines`;
}

export function ProjectCronsEditor({ slug, initial, pipelines }: Props) {
  const [crons, setCrons] = useState<ProjectCron[]>(initial);
  const [draft, setDraft] = useState<DraftForm | null>(null);
  const [pending, startTransition] = useTransition();

  const startCreate = () => setDraft(blankDraft());
  const startEdit = (cron: ProjectCron) => setDraft(draftFromCron(cron, pipelines));
  const cancelDraft = () => setDraft(null);

  const togglePipeline = (id: string) => {
    setDraft((d) => {
      if (!d) return d;
      const next = new Set(d.pipelineIds);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return { ...d, pipelineIds: next };
    });
  };

  const save = () => {
    if (!draft) return;
    const trimmedName = draft.name.trim();
    const trimmedExpr = draft.expression.trim();
    if (!trimmedName) {
      toast.error("Name is required");
      return;
    }
    if (!trimmedExpr) {
      toast.error("Cron expression is required");
      return;
    }
    const pipelineIds = draft.fireAll ? [] : Array.from(draft.pipelineIds);
    if (!draft.fireAll && pipelineIds.length === 0) {
      toast.error("Select at least one pipeline or enable 'Fire all'");
      return;
    }

    startTransition(async () => {
      const body = {
        slug,
        name: trimmedName,
        expression: trimmedExpr,
        pipeline_ids: pipelineIds,
        enabled: draft.enabled,
      };
      const res = draft.id
        ? await updateProjectCron({ ...body, id: draft.id })
        : await createProjectCron(body);
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      toast.success(draft.id ? "Schedule updated" : "Schedule created");
      // Optimistic list update: for create, prepend a synthetic
      // entry; for edit, replace in place. Next page fetch will
      // correct any stale timestamp. Keeps the UI responsive
      // without a second round-trip after revalidate.
      setCrons((prev) => {
        if (draft.id) {
          return prev.map((c) =>
            c.id === draft.id
              ? {
                  ...c,
                  name: trimmedName,
                  expression: trimmedExpr,
                  pipeline_ids: pipelineIds,
                  enabled: draft.enabled,
                }
              : c,
          );
        }
        return [
          {
            id: "__optimistic__" + Date.now(),
            project_id: "",
            name: trimmedName,
            expression: trimmedExpr,
            pipeline_ids: pipelineIds,
            enabled: draft.enabled,
            created_at: new Date().toISOString(),
            updated_at: new Date().toISOString(),
          },
          ...prev,
        ];
      });
      setDraft(null);
    });
  };

  const remove = (cron: ProjectCron) => {
    if (!confirm(`Delete schedule "${cron.name}"?`)) return;
    startTransition(async () => {
      const res = await deleteProjectCron({ slug, id: cron.id });
      if (!res.ok) {
        toast.error(res.error);
        return;
      }
      setCrons((prev) => prev.filter((c) => c.id !== cron.id));
      toast.success("Schedule deleted");
    });
  };

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h4 className="text-base font-medium">Schedules</h4>
          <p className="text-sm text-muted-foreground">
            Fire one or more pipelines on a cron schedule, independent of any
            per-pipeline{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-xs">cron:</code>{" "}
            material in YAML.
          </p>
        </div>
        {!draft ? (
          <Button onClick={startCreate} disabled={pending}>
            <Plus className="mr-2 size-4" aria-hidden />
            Add schedule
          </Button>
        ) : null}
      </div>

      {crons.length === 0 && !draft ? (
        <Card>
          <CardContent className="py-8 text-center text-sm text-muted-foreground">
            No schedules yet. Add one to fire pipelines on a cron expression.
          </CardContent>
        </Card>
      ) : null}

      {crons.length > 0 ? (
        <div className="space-y-2">
          {crons.map((c) => (
            <Card key={c.id}>
              <CardContent className="flex items-center justify-between gap-4 py-4">
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2">
                    <Clock className="size-4 text-muted-foreground" aria-hidden />
                    <span className="truncate font-medium">{c.name}</span>
                    {!c.enabled ? (
                      <span className="rounded bg-muted px-2 py-0.5 text-xs text-muted-foreground">
                        disabled
                      </span>
                    ) : null}
                  </div>
                  <div className="mt-1 text-xs text-muted-foreground">
                    <code className="rounded bg-muted px-1 py-0.5 font-mono">
                      {c.expression}
                    </code>
                    {" · "}
                    {describeTargets(c, pipelines)}
                    {c.last_fired_at
                      ? ` · last fired ${new Date(c.last_fired_at).toLocaleString()}`
                      : " · never fired"}
                  </div>
                </div>
                <div className="flex items-center gap-1">
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => startEdit(c)}
                    disabled={pending}
                    aria-label={`Edit ${c.name}`}
                  >
                    <Pencil className="size-4" aria-hidden />
                  </Button>
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => remove(c)}
                    disabled={pending}
                    aria-label={`Delete ${c.name}`}
                  >
                    <Trash2 className="size-4" aria-hidden />
                  </Button>
                </div>
              </CardContent>
            </Card>
          ))}
        </div>
      ) : null}

      {draft ? (
        <Card>
          <CardContent className="space-y-4 py-4">
            <div className="flex items-center justify-between">
              <h5 className="text-sm font-medium">
                {draft.id ? "Edit schedule" : "New schedule"}
              </h5>
              <Button
                variant="ghost"
                size="sm"
                onClick={cancelDraft}
                disabled={pending}
              >
                <X className="size-4" aria-hidden />
              </Button>
            </div>

            <div className="space-y-2">
              <Label htmlFor="cron-name">Name</Label>
              <Input
                id="cron-name"
                value={draft.name}
                onChange={(e) =>
                  setDraft((d) => (d ? { ...d, name: e.target.value } : d))
                }
                placeholder="nightly build"
                disabled={pending}
              />
            </div>

            <div className="space-y-2">
              <Label htmlFor="cron-expr">Cron expression</Label>
              <Input
                id="cron-expr"
                value={draft.expression}
                onChange={(e) =>
                  setDraft((d) =>
                    d ? { ...d, expression: e.target.value } : d,
                  )
                }
                placeholder="0 2 * * *  (every day at 2am)"
                className="font-mono"
                disabled={pending}
              />
              <p className="text-xs text-muted-foreground">
                Standard 5-field cron: minute hour day-of-month month
                day-of-week. Macros like{" "}
                <code className="rounded bg-muted px-1 py-0.5">@daily</code>,{" "}
                <code className="rounded bg-muted px-1 py-0.5">@hourly</code>{" "}
                also work.
              </p>
            </div>

            <div className="space-y-2">
              <label className="flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  className="size-4 accent-primary"
                  checked={draft.fireAll}
                  onChange={(e) =>
                    setDraft((d) =>
                      d ? { ...d, fireAll: e.target.checked } : d,
                    )
                  }
                  disabled={pending}
                />
                Fire all pipelines in this project (including ones added
                later)
              </label>

              {!draft.fireAll ? (
                <div className="space-y-1 rounded-md border p-3">
                  <p className="text-xs text-muted-foreground">
                    Select pipelines to fire:
                  </p>
                  {pipelines.length === 0 ? (
                    <p className="text-xs text-muted-foreground">
                      No pipelines in this project yet.
                    </p>
                  ) : (
                    pipelines.map((p) => (
                      <label
                        key={p.id}
                        className="flex items-center gap-2 text-sm"
                      >
                        <input
                          type="checkbox"
                          className="size-4 accent-primary"
                          checked={draft.pipelineIds.has(p.id)}
                          onChange={() => togglePipeline(p.id)}
                          disabled={pending}
                        />
                        {p.name}
                      </label>
                    ))
                  )}
                </div>
              ) : null}
            </div>

            <div className="space-y-2">
              <label className="flex items-center gap-2 text-sm">
                <input
                  type="checkbox"
                  className="size-4 accent-primary"
                  checked={draft.enabled}
                  onChange={(e) =>
                    setDraft((d) =>
                      d ? { ...d, enabled: e.target.checked } : d,
                    )
                  }
                  disabled={pending}
                />
                Enabled
              </label>
            </div>

            <div className="flex items-center gap-2">
              <Button onClick={save} disabled={pending}>
                {pending ? (
                  <Loader2 className="mr-2 size-4 animate-spin" aria-hidden />
                ) : (
                  <Save className="mr-2 size-4" aria-hidden />
                )}
                Save
              </Button>
              <Button variant="ghost" onClick={cancelDraft} disabled={pending}>
                Cancel
              </Button>
            </div>
          </CardContent>
        </Card>
      ) : null}
    </div>
  );
}
