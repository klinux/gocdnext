"use client";

import { Loader2, X } from "lucide-react";
import dynamic from "next/dynamic";

import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import type { ComplianceFramework, CompliancePolicy } from "@/server/queries/admin";

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

const MODE_LABELS: Record<string, string> = {
  inject: "inject (append jobs)",
  override: "override (replace jobs)",
};

const SAMPLE_YAML = `stages: [_compliance_scan]
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
    configYaml: "",
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

function Field({
  label,
  htmlFor,
  hint,
  children,
}: {
  label: string;
  htmlFor?: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={htmlFor}>{label}</Label>
      {children}
      {hint ? <p className="text-xs text-muted-foreground">{hint}</p> : null}
    </div>
  );
}

export function PolicyForm({
  draft,
  setDraft,
  frameworks,
  pending,
  onSave,
  onCancel,
}: {
  draft: PolicyDraft;
  setDraft: (d: PolicyDraft) => void;
  frameworks: ComplianceFramework[];
  pending: boolean;
  onSave: () => void;
  onCancel: () => void;
}) {
  const toggleFramework = (id: string) => {
    const has = draft.frameworkIds.includes(id);
    setDraft({
      ...draft,
      frameworkIds: has
        ? draft.frameworkIds.filter((x) => x !== id)
        : [...draft.frameworkIds, id],
    });
  };

  return (
    <div className="grid gap-4 px-4 pb-6">
      <Field label="Name" htmlFor="pol-name">
        <Input
          id="pol-name"
          value={draft.name}
          onChange={(e) => setDraft({ ...draft, name: e.target.value })}
          placeholder="pci-scan"
          autoFocus
        />
      </Field>
      <Field label="Description" htmlFor="pol-desc">
        <Input
          id="pol-desc"
          value={draft.description}
          onChange={(e) => setDraft({ ...draft, description: e.target.value })}
          placeholder="Mandatory dependency scan"
        />
      </Field>

      <div className="flex items-center justify-between rounded-md border border-border p-3">
        <div>
          <Label htmlFor="pol-enabled">Enabled</Label>
          <p className="text-xs text-muted-foreground">
            Disabled policies are not enforced on any project.
          </p>
        </div>
        <Switch
          id="pol-enabled"
          checked={draft.enabled}
          onCheckedChange={(v) => setDraft({ ...draft, enabled: v })}
        />
      </div>

      <div className="grid gap-4 sm:grid-cols-2">
        <Field label="Mode" hint="inject appends; override replaces repo jobs.">
          <Select
            items={MODE_LABELS}
            value={draft.mode}
            onValueChange={(v) =>
              v && setDraft({ ...draft, mode: v as PolicyDraft["mode"] })
            }
          >
            <SelectTrigger aria-label="Mode" className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="inject">inject (append jobs)</SelectItem>
              <SelectItem value="override">override (replace jobs)</SelectItem>
            </SelectContent>
          </Select>
        </Field>
        <Field label="Priority" htmlFor="pol-priority" hint="Lower applies first.">
          <Input
            id="pol-priority"
            type="number"
            value={String(draft.priority)}
            onChange={(e) =>
              setDraft({ ...draft, priority: Number(e.target.value) || 0 })
            }
          />
        </Field>
      </div>

      <div className="flex items-center justify-between rounded-md border border-border p-3">
        <div>
          <Label htmlFor="pol-all">Applies to all projects</Label>
          <p className="text-xs text-muted-foreground">
            Enforce on every project, ignoring frameworks.
          </p>
        </div>
        <Switch
          id="pol-all"
          checked={draft.appliesToAll}
          onCheckedChange={(v) => setDraft({ ...draft, appliesToAll: v })}
        />
      </div>

      {!draft.appliesToAll ? (
        <Field
          label="Frameworks"
          hint="Projects carrying any selected framework are governed by this policy."
        >
          {frameworks.length === 0 ? (
            <p className="text-xs text-muted-foreground">
              No frameworks yet — create one first.
            </p>
          ) : (
            <div className="flex flex-wrap gap-1.5">
              {frameworks.map((f) => {
                const on = draft.frameworkIds.includes(f.id);
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
          )}
        </Field>
      ) : null}

      <div className="grid gap-4 sm:grid-cols-2">
        <Field label="Position before" htmlFor="pol-before" hint="Stage to insert before.">
          <Input
            id="pol-before"
            value={draft.positionBefore}
            onChange={(e) =>
              setDraft({ ...draft, positionBefore: e.target.value, positionAfter: "" })
            }
            placeholder="deploy"
          />
        </Field>
        <Field label="Position after" htmlFor="pol-after" hint="Mutually exclusive.">
          <Input
            id="pol-after"
            value={draft.positionAfter}
            onChange={(e) =>
              setDraft({ ...draft, positionAfter: e.target.value, positionBefore: "" })
            }
            placeholder="build"
          />
        </Field>
      </div>

      <Field
        label="Policy config (YAML)"
        htmlFor="pol-yaml"
        hint="Pipeline schema. Stage & job names must start with _compliance_."
      >
        <PolicyYamlEditor
          id="pol-yaml"
          value={draft.configYaml}
          onChange={(value) => setDraft({ ...draft, configYaml: value })}
          placeholder={SAMPLE_YAML}
        />
      </Field>

      <div className="mt-2 flex items-center justify-end gap-2">
        <Button variant="ghost" onClick={onCancel} disabled={pending}>
          <X className="mr-1.5 size-4" /> Cancel
        </Button>
        <Button onClick={onSave} disabled={pending}>
          {pending ? <Loader2 className="mr-1.5 size-4 animate-spin" /> : null}
          Save
        </Button>
      </div>
    </div>
  );
}
