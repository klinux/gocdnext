import type { ReactNode } from "react";
import { GitCompareArrows, Info, Layers, Rocket } from "lucide-react";

import { cn } from "@/lib/utils";
import { shortHash, statusFor } from "@/lib/rollouts";
import type { Rollout } from "@/types/api";

import { AnalysisPanel } from "./analysis-panel";
import { RevisionStrip } from "./revision-strip";
import { StatusPill } from "./status-pill";
import { StepsTimeline } from "./steps-timeline";
import { TrafficBar } from "./traffic-bar";

function StrategyPill({ strategy }: { strategy: string }) {
  const canary = strategy === "canary";
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 font-mono text-[10.5px] font-bold uppercase tracking-wider",
        canary
          ? "border-teal-500/40 bg-teal-500/10 text-teal-600 dark:text-teal-400"
          : "border-sky-500/40 bg-sky-500/10 text-sky-600 dark:text-sky-400",
      )}
    >
      {canary ? (
        <Rocket className="size-3.5" aria-hidden />
      ) : (
        <Layers className="size-3.5" aria-hidden />
      )}
      {canary ? "Canary" : "Blue-Green"}
    </span>
  );
}

function Meta({ rollout }: { rollout: Rollout }) {
  const parts: string[] = [`ns/${rollout.namespace}`, shortHash(rollout.pod_hash)];
  if (rollout.strategy === "canary" && rollout.steps.length > 0) {
    const total = rollout.steps.length;
    const cur = rollout.current_step_known
      ? `${Math.min(rollout.current_step_index + 1, total)}`
      : "?";
    parts.push(`step ${cur}/${total}`);
  }
  return (
    <div className="flex flex-wrap items-center gap-2 font-mono text-[11.5px] text-muted-foreground">
      {parts.map((p, i) => (
        <span key={`${i}-${p}`} className="flex items-center gap-2">
          {i > 0 ? (
            <span className="opacity-40" aria-hidden>
              ·
            </span>
          ) : null}
          {p}
        </span>
      ))}
    </div>
  );
}

function SectionLabel({ children }: { children: ReactNode }) {
  return (
    <div className="mb-3 flex items-center gap-2.5 font-mono text-[10.5px] font-semibold uppercase tracking-wide text-muted-foreground">
      {children}
      <span className="h-px flex-1 bg-border" aria-hidden />
    </div>
  );
}

function CanaryBody({ rollout }: { rollout: Rollout }) {
  return (
    <div className="space-y-6">
      <section>
        <SectionLabel>Revisions &amp; traffic</SectionLabel>
        <RevisionStrip rollout={rollout} />
      </section>

      <TrafficBar
        canaryWeight={rollout.canary_weight}
        stableHash={rollout.stable_hash}
        podHash={rollout.pod_hash}
      />

      <div className="grid gap-6 lg:grid-cols-[1.5fr_1fr] lg:items-start">
        <section>
          <SectionLabel>Rollout steps</SectionLabel>
          <StepsTimeline rollout={rollout} />
          {rollout.message ? (
            <p className="mt-4 flex items-start gap-2 rounded-lg border border-border bg-muted/40 p-3 text-xs text-muted-foreground">
              <Info className="mt-0.5 size-4 shrink-0" aria-hidden />
              <span>{rollout.message}</span>
            </p>
          ) : null}
        </section>
        <section>
          <SectionLabel>Analysis (AnalysisRun)</SectionLabel>
          <AnalysisPanel analysis={rollout.analysis} />
        </section>
      </div>
    </div>
  );
}

// BlueGreenPlaceholder is intentionally compact: the active/preview blocks and
// traffic-swap detail land in PR-D. Read-only status only here.
function BlueGreenPlaceholder({ rollout }: { rollout: Rollout }) {
  const { label } = statusFor(rollout.phase, rollout.aborted);
  return (
    <div className="flex flex-col items-start gap-3 rounded-xl border border-dashed border-border bg-muted/20 p-5">
      <div className="flex items-center gap-2 text-sm font-medium">
        <GitCompareArrows className="size-4 text-sky-500" aria-hidden />
        Blue-green active / preview view is coming
      </div>
      <p className="text-sm text-muted-foreground">
        The active vs preview blocks and the traffic swap land in a later slice.
        For now this rollout is{" "}
        <span className="font-medium text-foreground">{label}</span>
        {rollout.message ? ` — ${rollout.message}` : ""}.
      </p>
    </div>
  );
}

type Props = { rollout: Rollout };

// RolloutPanel is one rollout: header (strategy pill, name + meta, status pill)
// over a strategy-specific body. Canary renders the full read-only view; blue-
// green renders a compact placeholder (PR-D). A paused canary gets a teal accent
// bar (it is the one awaiting an operator decision).
export function RolloutPanel({ rollout }: Props) {
  const canary = rollout.strategy === "canary";
  const attn = canary && rollout.phase === "Paused" && !rollout.aborted;
  return (
    <article
      className={cn(
        "overflow-hidden rounded-2xl border border-border bg-card",
        attn ? "border-l-2 border-l-teal-500" : "",
      )}
    >
      <header className="flex flex-wrap items-center gap-3.5 border-b border-border/70 px-5 py-4">
        <StrategyPill strategy={rollout.strategy} />
        <div className="flex min-w-0 flex-col gap-1">
          <h3 className="truncate text-base font-bold tracking-tight">
            {rollout.name}
          </h3>
          <Meta rollout={rollout} />
        </div>
        <div className="ml-auto">
          <StatusPill phase={rollout.phase} aborted={rollout.aborted} />
        </div>
      </header>
      <div className="p-5">
        {canary ? (
          <CanaryBody rollout={rollout} />
        ) : (
          <BlueGreenPlaceholder rollout={rollout} />
        )}
      </div>
    </article>
  );
}
