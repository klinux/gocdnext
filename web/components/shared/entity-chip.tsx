import Link from "next/link";
import type { Route } from "next";
import {
  Activity,
  ArrowLeft,
  ArrowRight,
  Box,
  GitPullRequest,
  KeyRound,
  Server,
  Shield,
  ShieldCheck,
  User,
  Users,
  Workflow,
} from "lucide-react";
import type { LucideIcon } from "lucide-react";

import { cn } from "@/lib/utils";

// EntityChip is the universal "this is a typed reference to <entity>"
// pill. Pulled out of the various ad-hoc renderings (UpstreamPills on
// the pipeline card, UpstreamBanner on the run page, plain text on
// the audit log) so cross-entity references read with the same visual
// weight everywhere — quick to scan, click to navigate, type encoded
// by colour + icon.
//
// Tone choice: each entity type carries a subtle bg-tinted variant
// from the brand palette. The colours are deliberately quiet — the
// chip should label without shouting; bright primary stays reserved
// for status (success/failed/queued) so the eye still treats those as
// the highest-priority signal.

export type EntityKind =
  | "pipeline"
  | "run"
  | "project"
  | "profile"
  | "user"
  | "group"
  | "service_account"
  | "secret"
  | "agent"
  | "pull_request";

const KIND_VISUALS: Record<EntityKind, { icon: LucideIcon; tone: string }> = {
  pipeline: {
    icon: Workflow,
    tone: "border-sky-500/30 bg-sky-500/10 text-sky-700 dark:text-sky-400",
  },
  run: {
    icon: Activity,
    tone: "border-violet-500/30 bg-violet-500/10 text-violet-700 dark:text-violet-400",
  },
  project: {
    icon: Box,
    tone: "border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-400",
  },
  profile: {
    icon: Server,
    tone: "border-teal-500/30 bg-teal-500/10 text-teal-700 dark:text-teal-400",
  },
  user: {
    icon: User,
    tone: "border-slate-500/30 bg-slate-500/10 text-slate-700 dark:text-slate-300",
  },
  group: {
    icon: Users,
    tone: "border-indigo-500/30 bg-indigo-500/10 text-indigo-700 dark:text-indigo-400",
  },
  service_account: {
    icon: ShieldCheck,
    tone: "border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400",
  },
  secret: {
    icon: KeyRound,
    tone: "border-rose-500/30 bg-rose-500/10 text-rose-700 dark:text-rose-400",
  },
  agent: {
    icon: Shield,
    tone: "border-cyan-500/30 bg-cyan-500/10 text-cyan-700 dark:text-cyan-400",
  },
  pull_request: {
    icon: GitPullRequest,
    tone: "border-fuchsia-500/30 bg-fuchsia-500/10 text-fuchsia-700 dark:text-fuchsia-400",
  },
};

type Direction = "in" | "out";

type Props = {
  kind: EntityKind;
  label: string;
  /** Optional secondary label (stage name, run counter, etc.) printed in dim. */
  hint?: string;
  /** When provided the chip renders as a link; otherwise it's static. */
  href?: Route;
  /** External href — opens in a new tab. Mutually exclusive with `href`. */
  externalHref?: string;
  /** Adds a directional arrow inside the chip; useful for relationships. */
  direction?: Direction;
  /** Tooltip-like title attribute fallback for hover. */
  title?: string;
  className?: string;
};

export function EntityChip({
  kind,
  label,
  hint,
  href,
  externalHref,
  direction,
  title,
  className,
}: Props) {
  const visual = KIND_VISUALS[kind];
  const Icon = visual.icon;
  const inner = (
    <>
      {direction === "in" ? <ArrowLeft className="size-3 shrink-0" aria-hidden /> : null}
      <Icon className="size-3 shrink-0" aria-hidden />
      <span className="truncate font-mono">{label}</span>
      {hint ? (
        <span className="shrink-0 opacity-70 font-mono">{hint}</span>
      ) : null}
      {direction === "out" ? <ArrowRight className="size-3 shrink-0" aria-hidden /> : null}
    </>
  );
  const base = cn(
    "inline-flex max-w-full items-center gap-1 rounded-full border px-2 py-0.5 text-[11px] leading-tight",
    visual.tone,
    href || externalHref ? "transition-colors hover:brightness-110" : null,
    className,
  );
  if (externalHref) {
    return (
      <a
        href={externalHref}
        target="_blank"
        rel="noreferrer noopener"
        className={base}
        title={title ?? label}
      >
        {inner}
      </a>
    );
  }
  if (href) {
    return (
      <Link href={href} className={base} title={title ?? label}>
        {inner}
      </Link>
    );
  }
  return (
    <span className={base} title={title ?? label}>
      {inner}
    </span>
  );
}

// EntityChipOverflow is the "+N more" companion — same chip shell,
// neutral tone, used when a list would otherwise wrap. Works as a
// child of EntityChipGroup or standalone.
export function EntityChipOverflow({
  count,
  title,
}: {
  count: number;
  title?: string;
}) {
  return (
    <span
      className="inline-flex items-center gap-1 rounded-full border border-border bg-muted px-2 py-0.5 text-[11px] leading-tight text-muted-foreground"
      title={title}
    >
      +{count}
    </span>
  );
}
