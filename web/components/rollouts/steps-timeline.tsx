import { Check, Circle, CircleDot, Pause } from "lucide-react";

import { cn } from "@/lib/utils";
import {
  isManualGate,
  stepLabel,
  stepState,
  type StepState,
} from "@/lib/rollouts";
import type { Rollout, RolloutStep } from "@/types/api";

const NODE_TONE: Record<StepState, string> = {
  done: "border-emerald-500/50 bg-emerald-500/10 text-emerald-500",
  current: "border-teal-500 bg-teal-500/10 text-teal-500 ring-4 ring-teal-500/15",
  pending: "border-border bg-muted text-muted-foreground",
};

function StepIcon({ state, gate }: { state: StepState; gate: boolean }) {
  if (gate) return <Pause className="size-3.5" aria-hidden />;
  if (state === "done") return <Check className="size-4" aria-hidden />;
  if (state === "current") return <CircleDot className="size-4" aria-hidden />;
  return <Circle className="size-3" aria-hidden />;
}

function StepNode({ step, state }: { step: RolloutStep; state: StepState }) {
  const gate = isManualGate(step);
  const isCurrentGate = state === "current" && gate;
  return (
    <span
      className={cn(
        "relative z-10 flex size-[30px] shrink-0 items-center justify-center rounded-full border-2",
        isCurrentGate
          ? "border-amber-500 bg-amber-500/10 text-amber-500 ring-4 ring-amber-500/15"
          : NODE_TONE[state],
      )}
    >
      <StepIcon state={state} gate={gate} />
    </span>
  );
}

type Props = { rollout: Rollout };

// StepsTimeline is the canary centerpiece: one node per step with done/current/
// pending states, an indefinite pause (`pause: {}`) rendered as an amber "manual"
// gate distinct from a timed pause, a "you are here" badge over the current node,
// and no highlight at all when the controller has not reported the index. Each
// node exposes an accessible name of "<kind> <label>, <state>" for assistive tech.
export function StepsTimeline({ rollout }: Props) {
  const { steps, current_step_index, current_step_known, aborted } = rollout;

  if (steps.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">
        This rollout defines no canary steps.
      </p>
    );
  }

  return (
    <div className="overflow-x-auto pb-1">
      <ol
        role="list"
        aria-label="Rollout steps"
        className="flex min-w-max items-start pt-5"
      >
        {steps.map((step, i) => {
          const state = stepState(
            i,
            current_step_index,
            current_step_known,
            aborted,
          );
          const gate = isManualGate(step);
          const label = stepLabel(step);
          const last = i === steps.length - 1;
          return (
            <li
              key={`${step.kind}-${i}`}
              aria-label={`${step.kind} ${label}, ${state}`}
              className="relative flex w-24 flex-col items-center gap-2"
            >
              {state === "current" ? (
                <span className="absolute -top-4 font-mono text-[9px] font-bold uppercase tracking-wide text-amber-500">
                  you are here
                </span>
              ) : null}
              {!last ? (
                <span
                  aria-hidden
                  className={cn(
                    "absolute top-[14px] left-[calc(50%+15px)] h-0.5 w-[calc(100%-30px)]",
                    state === "done" ? "bg-emerald-500/50" : "bg-border",
                  )}
                />
              ) : null}
              <StepNode step={step} state={state} />
              <span className="font-mono text-[9px] uppercase tracking-wide text-muted-foreground">
                {step.kind}
              </span>
              <span
                className={cn(
                  "text-center font-mono text-[11px] font-semibold leading-tight",
                  state === "current"
                    ? "text-foreground"
                    : gate
                      ? "text-amber-500"
                      : "text-muted-foreground",
                )}
              >
                {label}
              </span>
            </li>
          );
        })}
      </ol>
    </div>
  );
}
