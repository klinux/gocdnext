"use client";

import dynamic from "next/dynamic";
import { Check } from "lucide-react";

import { Input } from "@/components/ui/input";
import type { ComplianceFramework, CompliancePolicy } from "@/server/queries/admin";
import {
  FieldLabel,
  FrameworkChips,
  Hint,
  ModeHint,
  ModeSegmented,
  PlacementRail,
  PriorityStepper,
  ScopeNote,
  SectionLabel,
  ToggleCard,
} from "./policy-form-parts";

// CodeMirror is browser-only and heavy — load it lazily so it stays off the
// server render and the initial client chunk.
const PolicyYamlEditor = dynamic(() => import("./policy-yaml-editor.client"), {
  ssr: false,
  loading: () => (
    <div className="h-64 animate-pulse rounded-md border border-input bg-muted/30" />
  ),
});

export type PolicyDraft = {
  id: string | null;
  name: string;
  description: string;
  enabled: boolean;
  mode: "inject" | "override";
  priority: number;
  appliesToAll: boolean;
  positionBefore: string;
  positionAfter: string;
  frameworkIds: string[];
  configYaml: string;
};

export const SAMPLE_YAML = `stages: [_compliance_scan]
jobs:
  _compliance_scan:
    stage: _compliance_scan
    image: scanner:latest
    script: ["scan ."]`;

export function blankPolicy(): PolicyDraft {
  return {
    id: null,
    name: "",
    description: "",
    enabled: true,
    mode: "inject",
    priority: 0,
    appliesToAll: false,
    positionBefore: "",
    positionAfter: "",
    frameworkIds: [],
    // Start from the sample so the editor and the live preview have something
    // to render immediately — the admin edits it down to their real policy.
    configYaml: SAMPLE_YAML,
  };
}

export function policyToDraft(p: CompliancePolicy): PolicyDraft {
  return {
    id: p.id,
    name: p.name,
    description: p.description,
    enabled: p.enabled,
    mode: p.mode,
    priority: p.priority,
    appliesToAll: p.applies_to_all,
    positionBefore: p.position_before,
    positionAfter: p.position_after,
    frameworkIds: p.framework_ids ?? [],
    configYaml: p.config_yaml,
  };
}

const sectionCls = "border-t border-border/60 py-5 first:border-t-0 first:pt-1";

export function PolicyForm({
  draft,
  setDraft,
  frameworks,
  baseStages,
}: {
  draft: PolicyDraft;
  setDraft: (d: PolicyDraft) => void;
  frameworks: ComplianceFramework[];
  baseStages: string[];
}) {
  const set = (patch: Partial<PolicyDraft>) => setDraft({ ...draft, ...patch });
  const toggleFramework = (id: string) =>
    set({
      frameworkIds: draft.frameworkIds.includes(id)
        ? draft.frameworkIds.filter((x) => x !== id)
        : [...draft.frameworkIds, id],
    });

  return (
    <div className="pb-2">
      {/* 1 — Identity */}
      <section className={sectionCls}>
        <SectionLabel n={1}>Identity</SectionLabel>
        <div className="grid grid-cols-2 gap-3.5">
          <div>
            <FieldLabel>Name</FieldLabel>
            <Input
              aria-label="Name"
              value={draft.name}
              onChange={(e) => set({ name: e.target.value })}
              placeholder="pci-scan"
              className="font-mono"
              autoFocus
            />
          </div>
          <div>
            <FieldLabel optional>Description</FieldLabel>
            <Input
              aria-label="Description"
              value={draft.description}
              onChange={(e) => set({ description: e.target.value })}
              placeholder="What this enforces"
            />
          </div>
        </div>
        <div className="mt-3.5">
          <ToggleCard
            id="pol-enabled"
            title="Enabled"
            sub="Disabled policies stay saved but are not enforced on any project."
            checked={draft.enabled}
            onChange={(v) => set({ enabled: v })}
          />
        </div>
      </section>

      {/* 2 — Behavior */}
      <section className={sectionCls}>
        <SectionLabel n={2}>Behavior</SectionLabel>
        <div className="mb-4">
          <FieldLabel>Mode</FieldLabel>
          <ModeSegmented value={draft.mode} onChange={(mode) => set({ mode })} />
          <ModeHint mode={draft.mode} />
        </div>
        <div>
          <FieldLabel>Priority</FieldLabel>
          <PriorityStepper value={draft.priority} onChange={(priority) => set({ priority })} />
          <Hint>Lower numbers apply first when several policies target the same project.</Hint>
        </div>
      </section>

      {/* 3 — Scope */}
      <section className={sectionCls}>
        <SectionLabel n={3}>Scope</SectionLabel>
        <ToggleCard
          id="pol-all"
          title="Applies to all projects"
          sub="Enforce everywhere and ignore framework matching."
          checked={draft.appliesToAll}
          onChange={(v) => set({ appliesToAll: v })}
        />
        {draft.appliesToAll ? (
          <div className="mt-3">
            <ScopeNote />
          </div>
        ) : (
          <div className="mt-4">
            <FieldLabel>Frameworks</FieldLabel>
            <FrameworkChips
              frameworks={frameworks}
              selected={draft.frameworkIds}
              onToggle={toggleFramework}
            />
            <Hint>
              Projects carrying <b className="text-muted-foreground">any</b> selected framework
              are governed by this policy.
            </Hint>
          </div>
        )}
      </section>

      {/* 4 — Placement */}
      <section className={sectionCls}>
        <SectionLabel n={4}>Placement</SectionLabel>
        <FieldLabel>Where the stage lands</FieldLabel>
        <p className="mb-2.5 text-[11.5px] text-muted-foreground/80">
          Click a gap in the project&apos;s stage order to insert the compliance stage.
        </p>
        <PlacementRail
          stages={baseStages}
          positionBefore={draft.positionBefore}
          positionAfter={draft.positionAfter}
          onChange={(before, after) => set({ positionBefore: before, positionAfter: after })}
        />
      </section>

      {/* 5 — Definition */}
      <section className={sectionCls}>
        <SectionLabel n={5}>Definition</SectionLabel>
        <FieldLabel>Policy config</FieldLabel>
        <PolicyYamlEditor
          id="pol-yaml"
          value={draft.configYaml}
          onChange={(value) => set({ configYaml: value })}
          placeholder={SAMPLE_YAML}
        />
        <div className="mt-2.5 flex flex-wrap items-center gap-4 text-[11.5px] text-muted-foreground">
          <span className="flex items-center gap-1.5">
            <Check className="size-3.5 text-emerald-500" />
            Stage &amp; job names prefixed{" "}
            <span className="font-mono text-primary">_compliance_</span>
          </span>
        </div>
      </section>
    </div>
  );
}
