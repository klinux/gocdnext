"use client";

import { ArrowRight, Info, Plus, TriangleAlert } from "lucide-react";

import { cn } from "@/lib/utils";
import { Switch } from "@/components/ui/switch";

// Numbered section header — mono uppercase label + a small numbered chip,
// matching the handoff's `.sec-label`.
export function SectionLabel({ n, children }: { n: number; children: React.ReactNode }) {
  return (
    <div className="mb-4 flex items-center gap-2.5 font-mono text-[10.5px] font-semibold uppercase tracking-wider text-muted-foreground">
      <span className="flex size-[17px] items-center justify-center rounded-[5px] border border-border bg-muted text-[9.5px]">
        {n}
      </span>
      {children}
    </div>
  );
}

export function FieldLabel({
  children,
  optional,
}: {
  children: React.ReactNode;
  optional?: boolean;
}) {
  return (
    <div className="mb-1.5 flex items-center gap-2 text-[12.5px] font-semibold">
      {children}
      {optional ? (
        <span className="font-mono text-[11px] font-medium text-muted-foreground/70">
          optional
        </span>
      ) : null}
    </div>
  );
}

export function Hint({ children }: { children: React.ReactNode }) {
  return <p className="mt-1.5 text-[11.5px] leading-snug text-muted-foreground/80">{children}</p>;
}

// Toggle card — title + sub + a Switch; the `on` state tints the border teal.
export function ToggleCard({
  title,
  sub,
  checked,
  onChange,
  id,
}: {
  title: string;
  sub: string;
  checked: boolean;
  onChange: (v: boolean) => void;
  id: string;
}) {
  return (
    <div
      className={cn(
        "flex items-center gap-3.5 rounded-xl border p-3.5 transition-colors",
        checked
          ? "border-primary/35 bg-gradient-to-b from-primary/10 to-transparent"
          : "border-border bg-card",
      )}
    >
      <div className="flex-1">
        <label htmlFor={id} className="text-[13px] font-semibold">
          {title}
        </label>
        <p className="mt-0.5 text-[11.5px] leading-snug text-muted-foreground">{sub}</p>
      </div>
      <Switch id={id} checked={checked} onCheckedChange={onChange} />
    </div>
  );
}

// Segmented control for the inject/override mode.
export function ModeSegmented({
  value,
  onChange,
}: {
  value: "inject" | "override";
  onChange: (m: "inject" | "override") => void;
}) {
  const opts = [
    { key: "inject" as const, label: "Inject", Icon: Plus },
    { key: "override" as const, label: "Override", Icon: ArrowRight },
  ];
  return (
    <div className="flex gap-1 rounded-xl border border-border bg-background p-1">
      {opts.map(({ key, label, Icon }) => {
        const on = value === key;
        return (
          <button
            key={key}
            type="button"
            onClick={() => onChange(key)}
            aria-pressed={on}
            className={cn(
              "flex flex-1 items-center justify-center gap-2 rounded-lg px-2.5 py-2 text-[12.5px] font-semibold transition-colors",
              on
                ? "bg-primary/10 text-primary shadow-[inset_0_0_0_1px] shadow-primary/35"
                : "text-muted-foreground hover:text-foreground",
            )}
          >
            <Icon className="size-3.5" />
            {label}
          </button>
        );
      })}
    </div>
  );
}

export function ModeHint({ mode }: { mode: "inject" | "override" }) {
  return (
    <div className="mt-2.5 flex items-start gap-2 text-[11.5px] leading-snug text-muted-foreground">
      <Info className="mt-px size-3.5 shrink-0 text-primary" />
      <span>
        {mode === "inject" ? (
          <>
            <b className="font-semibold text-foreground">Inject</b> appends the policy jobs
            alongside the repo&apos;s own jobs. Nothing in the project pipeline is replaced.
          </>
        ) : (
          <>
            <b className="font-semibold text-foreground">Override</b> replaces the matched
            project&apos;s jobs with the policy jobs. Use only for hard lockdown — repo CI is
            discarded.
          </>
        )}
      </span>
    </div>
  );
}

// Priority stepper (– value +), clamped 0–99.
export function PriorityStepper({
  value,
  onChange,
}: {
  value: number;
  onChange: (v: number) => void;
}) {
  const step = (d: number) => onChange(Math.max(0, Math.min(99, value + d)));
  return (
    <div className="flex items-stretch overflow-hidden rounded-[10px] border border-border bg-background">
      <button
        type="button"
        onClick={() => step(-1)}
        aria-label="Decrease priority"
        className="w-10 bg-muted text-lg text-muted-foreground transition-colors hover:bg-accent hover:text-primary"
      >
        –
      </button>
      <div className="flex-1 self-center text-center font-mono text-[15px] font-semibold">
        {value}
      </div>
      <button
        type="button"
        onClick={() => step(1)}
        aria-label="Increase priority"
        className="w-10 bg-muted text-lg text-muted-foreground transition-colors hover:bg-accent hover:text-primary"
      >
        +
      </button>
    </div>
  );
}

export function ScopeNote() {
  return (
    <div className="mb-3 flex items-center gap-2 text-[11.5px] text-amber-600 dark:text-amber-400">
      <TriangleAlert className="size-3.5 shrink-0" />
      Framework matching is bypassed — this hits every project in the org.
    </div>
  );
}

// Framework multi-select chips.
export function FrameworkChips({
  frameworks,
  selected,
  onToggle,
}: {
  frameworks: { id: string; name: string }[];
  selected: string[];
  onToggle: (id: string) => void;
}) {
  if (frameworks.length === 0) {
    return (
      <div className="rounded-xl border border-dashed border-border bg-card p-3.5 text-[12px] text-muted-foreground">
        No frameworks yet — create one first.
      </div>
    );
  }
  return (
    <div className="flex flex-wrap gap-2">
      {frameworks.map((f) => {
        const on = selected.includes(f.id);
        return (
          <button
            key={f.id}
            type="button"
            onClick={() => onToggle(f.id)}
            aria-pressed={on}
            aria-label={`Framework ${f.name}`}
            className={cn(
              "flex items-center gap-2 rounded-lg border px-3 py-1.5 font-mono text-[11.5px] font-medium transition-colors",
              on
                ? "border-primary/35 bg-primary/10 text-primary"
                : "border-border bg-background text-muted-foreground hover:text-foreground",
            )}
          >
            <span
              className={cn("size-[7px] rounded-full", on ? "bg-primary" : "bg-border")}
            />
            {f.name}
          </button>
        );
      })}
    </div>
  );
}

// Placement rail — the real project stages with clickable gaps; the selected gap
// shows the compliance chip. Maps gap index ↔ position_before/after.
export function PlacementRail({
  stages,
  positionBefore,
  positionAfter,
  onChange,
}: {
  stages: string[];
  positionBefore: string;
  positionAfter: string;
  onChange: (before: string, after: string) => void;
}) {
  if (stages.length === 0) {
    return (
      <div className="rounded-xl border border-border bg-background p-3.5 text-[11.5px] text-muted-foreground">
        Pick a project and write a valid config to choose where the stage lands.
      </div>
    );
  }
  // No explicit anchor → gap 0, matching the backend's prepend default
  // (insertStages uses idx 0 when before/after are empty).
  const selected = positionBefore
    ? stages.indexOf(positionBefore)
    : positionAfter
      ? stages.indexOf(positionAfter) + 1
      : 0;

  const pick = (idx: number) => {
    if (idx < stages.length) onChange(stages[idx]!, "");
    else onChange("", stages[stages.length - 1]!);
  };

  // Render helper (not a nested component) — keeps the per-gap closures without
  // tripping react-hooks/static-components.
  const gap = (idx: number) => (
    <button
      key={`gap-${idx}`}
      type="button"
      onClick={() => pick(idx)}
      className="group relative flex min-h-[22px] w-full items-center pl-5 text-left"
    >
      <span className="absolute left-[3.5px] top-0 bottom-0 w-0.5 bg-border" aria-hidden />
      {idx === selected ? (
        <span className="rounded-md border border-primary/35 bg-primary/10 px-2.5 py-0.5 font-mono text-[11px] font-semibold text-primary">
          _compliance_*
        </span>
      ) : (
        <span className="font-mono text-[10.5px] text-muted-foreground/50 opacity-0 transition-opacity group-hover:text-primary group-hover:opacity-100">
          + insert here
        </span>
      )}
    </button>
  );

  return (
    <div className="flex flex-col rounded-xl border border-border bg-background px-3.5 py-2">
      {gap(0)}
      {stages.map((s, i) => (
        <div key={s}>
          <div className="flex items-center gap-2.5 py-2">
            <span className="size-[9px] rounded-full border-2 border-muted-foreground/60 bg-background" />
            <span className="font-mono text-[12.5px] font-semibold">{s}</span>
          </div>
          {gap(i + 1)}
        </div>
      ))}
    </div>
  );
}
