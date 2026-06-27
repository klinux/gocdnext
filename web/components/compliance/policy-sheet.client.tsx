"use client";

import { useMemo } from "react";
import { Check, Loader2, ShieldCheck, Sparkles } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { cn } from "@/lib/utils";
import type { ComplianceFramework } from "@/server/queries/admin";
import { PolicyForm, type PolicyDraft } from "./policy-form.client";
import { PolicyPreview } from "./policy-preview.client";
import { applyTemplate, POLICY_TEMPLATES } from "./policy-templates";
import { usePolicyPreview, type PreviewProject } from "./use-policy-preview";

// Thin custom scrollbar matching the handoff (default browser bars look heavy).
const scrollbar =
  "[&::-webkit-scrollbar]:w-2 [&::-webkit-scrollbar-track]:bg-transparent [&::-webkit-scrollbar-thumb]:rounded-full [&::-webkit-scrollbar-thumb]:bg-border hover:[&::-webkit-scrollbar-thumb]:bg-muted-foreground/30";

// PolicySheet is the two-pane authoring experience: pinned header (icon + step
// trail) over a scrolling form (left) and the live merge preview (right), with a
// pinned footer carrying the summary + primary action. Rendered only when a
// draft exists, so the preview hook always has one.
export function PolicySheet({
  draft,
  setDraft,
  frameworks,
  projects,
  pending,
  onSave,
  onCancel,
}: {
  draft: PolicyDraft;
  setDraft: (d: PolicyDraft) => void;
  frameworks: ComplianceFramework[];
  projects: PreviewProject[];
  pending: boolean;
  onSave: () => void;
  onCancel: () => void;
}) {
  const preview = usePolicyPreview(draft, projects);

  const fwName = useMemo(() => {
    const m = new Map<string, string>();
    for (const f of frameworks) m.set(f.id, f.name);
    return m;
  }, [frameworks]);
  const frameworkNames = draft.appliesToAll
    ? []
    : draft.frameworkIds.map((id) => fwName.get(id) ?? id);

  const touched = preview.views?.filter((v) => v.effective.stages.length > v.raw.stages.length).length ?? 0;
  const summary = draft.mode === "override" ? "Replaces jobs" : `Injects into ${touched} pipeline${touched === 1 ? "" : "s"}`;

  return (
    <div className="grid h-full min-h-0 grid-rows-[auto_1fr_auto]">
      {/* header */}
      <header className="flex items-start gap-3.5 border-b border-border/60 p-6 pb-4 pr-12">
        <span className="flex size-[38px] shrink-0 items-center justify-center rounded-xl border border-primary/35 bg-primary/10 text-primary">
          <ShieldCheck className="size-5" />
        </span>
        <div className="min-w-0">
          <h2 className="text-[18px] font-bold tracking-tight">
            {draft.id ? "Edit policy" : "New policy"}
          </h2>
          <p className="mt-0.5 text-[12.5px] leading-snug text-muted-foreground">
            Mandatory pipeline config merged into every targeted project at run time.
          </p>
          <div className="mt-3 flex items-center gap-1.5 font-mono text-[10.5px] uppercase tracking-wide text-muted-foreground">
            {["Define", "Scope", "Config"].map((s, i) => (
              <span key={s} className="flex items-center gap-1.5">
                {i > 0 ? <span className="h-px w-4 bg-border" /> : null}
                <span className="size-1.5 rounded-full bg-primary" />
                {s}
              </span>
            ))}
          </div>
        </div>
        {/* New policies can start from a ready-made template; editing keeps the
            existing config untouched. */}
        {draft.id ? null : (
          <DropdownMenu>
            <DropdownMenuTrigger
              render={<Button variant="outline" size="sm" className="ml-auto shrink-0" />}
            >
              <Sparkles className="mr-1.5 size-3.5" />
              Start from template
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-72">
              <DropdownMenuLabel>Start from a template</DropdownMenuLabel>
              {POLICY_TEMPLATES.map((t) => (
                <DropdownMenuItem
                  key={t.key}
                  onClick={() => setDraft(applyTemplate(draft, t))}
                  className="flex flex-col items-start gap-0.5"
                >
                  <span className="text-[13px] font-medium">{t.label}</span>
                  <span className="text-xs text-muted-foreground">{t.description}</span>
                </DropdownMenuItem>
              ))}
            </DropdownMenuContent>
          </DropdownMenu>
        )}
      </header>

      {/* two-pane body */}
      <div className="grid min-h-0 grid-cols-1 md:grid-cols-[1.1fr_0.9fr]">
        <div className={cn("min-h-0 overflow-y-auto px-6 md:border-r md:border-border/60", scrollbar)}>
          <PolicyForm
            draft={draft}
            setDraft={setDraft}
            frameworks={frameworks}
            baseStages={preview.baseStages}
          />
        </div>
        <aside className={cn("hidden min-h-0 overflow-y-auto bg-muted/15 p-5 md:block", scrollbar)}>
          <PolicyPreview
            draft={draft}
            projects={projects}
            frameworkNames={frameworkNames}
            preview={preview}
          />
        </aside>
      </div>

      {/* footer */}
      <footer className="flex items-center gap-3.5 border-t border-border/60 bg-background px-6 py-3.5">
        <span className="flex items-center gap-2 text-[12px] text-muted-foreground">
          <span className="flex size-[17px] items-center justify-center rounded-full bg-emerald-500/15 text-emerald-500">
            <Check className="size-3" />
          </span>
          {draft.enabled ? null : <span className="text-muted-foreground/60">(disabled) </span>}
          {summary}
        </span>
        <div className="ml-auto flex items-center gap-2.5">
          <Button variant="ghost" onClick={onCancel} disabled={pending}>
            Cancel
          </Button>
          <Button onClick={onSave} disabled={pending}>
            {pending ? <Loader2 className="mr-1.5 size-4 animate-spin" /> : <Check className="mr-1.5 size-4" />}
            {draft.id ? "Save changes" : "Create policy"}
          </Button>
        </div>
      </footer>
    </div>
  );
}
