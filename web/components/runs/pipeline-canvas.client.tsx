"use client";

import {
  Ban,
  CheckCircle2,
  ChevronRight,
  Clock,
  CircleDashed,
  Loader2,
  MinusCircle,
  Server,
  XCircle,
} from "lucide-react";
import { useQuery } from "@tanstack/react-query";
import type { ComponentType } from "react";

import { cn } from "@/lib/utils";
import { LiveDuration } from "@/components/shared/live-duration";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { fetchServices } from "@/components/runs/run-services.client";
import { isTerminalStatus } from "@/lib/status";
import type { JobDetail, RunService, StageDetail } from "@/types/api";

type Props = {
  stages: StageDetail[];
  runId: string;
  runStatus: string;
  apiBaseURL: string;
};

const SERVICES_POLL_MS = 3_000;

// PipelineCanvas is the "pipeline view" at the top of a run's
// detail page: stages as columns left-to-right, jobs as pills
// inside, chevron connectors between. Clicking a job scrolls the
// matching JobCard (rendered below via StageSection) into view —
// avoids a second log viewer that would lag the primary one.
//
// When the run has services declared, a virtual "Setup" column is
// rendered as the FIRST column (before stage 1) with one node per
// service. Anchoring services to the start of the graph matches
// their run-scoped lifetime (they come up before stage 1 even
// dispatches) and mirrors Woodpecker's pipeline view, which the
// operator already builds intuition around.

export function PipelineCanvas({ stages, runId, runStatus, apiBaseURL }: Props) {
  // Share the cache with RunTabs's services query so opening the
  // tab after the canvas mounts reads from cache instantly.
  const servicesQuery = useQuery({
    queryKey: ["run-services", runId],
    queryFn: () => fetchServices(apiBaseURL, runId),
    refetchInterval: isTerminalStatus(runStatus) ? false : SERVICES_POLL_MS,
    staleTime: 30_000,
  });
  const services = servicesQuery.data ?? [];

  if (stages.length === 0 && services.length === 0) {
    return null;
  }
  return (
    <section aria-label="Pipeline" className="-mx-2 overflow-x-auto px-2 pb-2">
      <ol className="flex min-w-full items-stretch gap-2">
        {services.length > 0 ? (
          <li className="flex items-stretch">
            <ServicesColumn services={services} />
            {stages.length > 0 ? (
              <Connector
                previousStatus={aggregateServicesStatus(services)}
                nextStatus={stages[0]!.status}
              />
            ) : null}
          </li>
        ) : null}
        {stages.map((stage, i) => (
          <li key={stage.id} className="flex items-stretch">
            <StageColumn stage={stage} />
            {i < stages.length - 1 ? (
              <Connector
                previousStatus={stage.status}
                nextStatus={stages[i + 1]!.status}
              />
            ) : null}
          </li>
        ))}
      </ol>
    </section>
  );
}

// aggregateServicesStatus picks the worst-case status across all
// service nodes so the connector between Setup and stage 1
// reflects the actual readiness of the prerequisite group.
// Precedence: failed > starting > everything-else (success).
//
// `stopped` is intentionally folded into success here, not into
// `skipped`. A clean run ends with the cleanup broadcast firing
// `stopped` on every service, so treating stopped as "dimmed"
// would paint a successful Setup as broken AFTER the run
// terminated. The Services tab uses the same "stopped = neutral
// done" semantic via StatusPill; this matches it.
function aggregateServicesStatus(services: RunService[]): string {
  let hasStarting = false;
  for (const svc of services) {
    if (svc.status === "failed") return "failed";
    if (svc.status === "starting") hasStarting = true;
  }
  if (hasStarting) return "running";
  return "success";
}

// --- services column (virtual "setup" stage) ---

function ServicesColumn({ services }: { services: RunService[] }) {
  const aggregate = aggregateServicesStatus(services);
  const tone = statusTone(aggregate);
  return (
    <div
      className={cn(
        "flex min-w-[220px] max-w-[260px] flex-col rounded-lg border bg-card",
        tone.border,
      )}
      data-status={aggregate}
      data-kind="services"
    >
      <header
        className={cn(
          "flex items-center gap-2 border-b px-3 py-2 text-xs font-medium",
          tone.header,
        )}
      >
        <Server className="size-3.5" aria-hidden />
        <span className="truncate">
          <span className="text-[10px] text-muted-foreground/80 mr-1">
            setup
          </span>
          services
        </span>
        <span className="ml-auto rounded-full bg-background/60 px-1.5 py-0.5 font-mono text-[10px] tabular-nums text-muted-foreground">
          {services.length}
        </span>
      </header>
      <div className="flex flex-col gap-1.5 p-2">
        {services.map((svc) => (
          <ServicePill key={svc.id} service={svc} />
        ))}
      </div>
    </div>
  );
}

// servicePillStatus maps the engine's service-side enum into the
// shared status tone vocabulary so the same TONE palette covers
// services + jobs + stages with no extra theme entries:
//   ready    → success
//   starting → running
//   stopped  → success (cleanup-on-terminal is the happy path;
//              v0.6.1 still surfaces a true failure via the
//              sticky `failed` status, so this fold can't hide
//              a service that actually died)
//   failed   → failed
function servicePillStatus(s: RunService["status"]): string {
  switch (s) {
    case "ready":
      return "success";
    case "starting":
      return "running";
    case "stopped":
      return "success";
    case "failed":
      return "failed";
    default:
      return "waiting";
  }
}

function ServicePill({ service }: { service: RunService }) {
  const status = servicePillStatus(service.status);
  const tone = statusTone(status);
  return (
    <Popover>
      <PopoverTrigger
        className={cn(
          "group flex items-center gap-1.5 rounded-md border px-2 py-1 text-left text-xs transition-colors hover:bg-muted/60 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/40",
          tone.pillBorder,
          tone.pillBg,
        )}
        title={service.error || undefined}
      >
        <StatusGlyph status={status} className={cn("size-3.5", tone.glyph)} />
        <span className={cn("flex-1 truncate font-mono", tone.text)}>
          {service.name}
        </span>
        <LiveDuration
          startedAt={service.started_at ?? null}
          finishedAt={service.stopped_at ?? service.ready_at ?? null}
          className="font-mono text-[10px] text-muted-foreground/80"
        />
      </PopoverTrigger>
      <PopoverContent
        align="start"
        sideOffset={6}
        className="w-80 text-xs"
      >
        <div className="space-y-2">
          <div className="flex items-center gap-2">
            <StatusGlyph status={status} className={cn("size-4", tone.glyph)} />
            <p className="font-mono text-sm font-semibold">{service.name}</p>
            <span
              className={cn(
                "ml-auto rounded-full px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide",
                tone.pillBg,
                tone.text,
              )}
            >
              {service.status}
            </span>
          </div>
          <p className="break-all font-mono text-[11px] text-muted-foreground">
            {service.image}
          </p>
          {service.pod_name ? (
            <p className="break-all font-mono text-[10px] text-muted-foreground/80">
              pod: {service.pod_name}
            </p>
          ) : null}
          <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
            <dt className="text-muted-foreground">started</dt>
            <dd className="font-mono">
              {fmtTs(service.started_at) ?? "—"}
            </dd>
            <dt className="text-muted-foreground">ready</dt>
            <dd className="font-mono">
              {fmtTs(service.ready_at) ?? "—"}
            </dd>
            <dt className="text-muted-foreground">stopped</dt>
            <dd className="font-mono">
              {fmtTs(service.stopped_at) ?? "—"}
            </dd>
          </dl>
          {service.error ? (
            <p
              className={cn(
                "rounded-md border px-2 py-1.5 font-mono text-[11px] break-words",
                "border-status-failed/40 bg-status-failed-bg text-status-failed-fg",
              )}
            >
              {service.error}
            </p>
          ) : null}
        </div>
      </PopoverContent>
    </Popover>
  );
}

function fmtTs(iso: string | null | undefined): string | null {
  if (!iso) return null;
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return null;
  return d.toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

// --- column ---

function StageColumn({ stage }: { stage: StageDetail }) {
  const tone = statusTone(stage.status);
  return (
    <div
      className={cn(
        "flex min-w-[220px] max-w-[260px] flex-col rounded-lg border bg-card",
        tone.border,
      )}
      data-status={stage.status}
    >
      <header
        className={cn(
          "flex items-center gap-2 border-b px-3 py-2 text-xs font-medium",
          tone.header,
        )}
      >
        <StatusGlyph status={stage.status} className="size-3.5" />
        <span className="truncate">
          <span className="text-[10px] text-muted-foreground/80 mr-1">
            #{stage.ordinal + 1}
          </span>
          {stage.name}
        </span>
        <LiveDuration
          startedAt={stage.started_at}
          finishedAt={stage.finished_at}
          className="ml-auto font-mono text-[10px] text-muted-foreground"
        />
      </header>
      <div className="flex flex-col gap-1.5 p-2">
        {stage.jobs.length === 0 ? (
          <p className="rounded-md border border-dashed px-2 py-1.5 text-center text-[11px] text-muted-foreground">
            no jobs
          </p>
        ) : (
          stage.jobs.map((j) => <JobPill key={j.id} job={j} />)
        )}
      </div>
    </div>
  );
}

// --- job pill ---

function JobPill({ job }: { job: JobDetail }) {
  const tone = statusTone(job.status);
  const label = job.matrix_key
    ? `${job.name} [${job.matrix_key}]`
    : job.name;

  const scrollToJob = () => {
    // The JobCard rendered below in StageSection carries id
    // job-<uuid>. We add that id in job-card.tsx.
    if (typeof document === "undefined") return;
    const target = document.getElementById(`job-${job.id}`);
    if (target) {
      target.scrollIntoView({ behavior: "smooth", block: "center" });
      target.classList.add("ring-2", "ring-primary/40");
      setTimeout(() => target.classList.remove("ring-2", "ring-primary/40"), 1200);
    }
  };

  return (
    <button
      type="button"
      onClick={scrollToJob}
      className={cn(
        "group flex items-center gap-1.5 rounded-md border px-2 py-1 text-left text-xs transition-colors hover:bg-muted/60 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/40",
        tone.pillBorder,
        tone.pillBg,
      )}
      title={job.error || undefined}
    >
      <StatusGlyph status={job.status} className={cn("size-3.5", tone.glyph)} />
      <span className={cn("flex-1 truncate font-mono", tone.text)}>{label}</span>
      <LiveDuration
        startedAt={job.started_at}
        finishedAt={job.finished_at}
        className="font-mono text-[10px] text-muted-foreground/80"
      />
    </button>
  );
}

// --- connector between columns ---

function Connector({
  previousStatus,
  nextStatus,
}: {
  previousStatus: string;
  nextStatus: string;
}) {
  // Connector color follows the downstream state: if the next
  // stage is running we want the flow-line to feel "active". If
  // previous failed + downstream is canceled, we dim the line to
  // communicate the blocked edge.
  const tone = statusTone(isDim(previousStatus) ? previousStatus : nextStatus);
  return (
    <span
      aria-hidden
      className={cn(
        "mx-1 flex shrink-0 items-center",
        previousStatus === "running" ? "text-primary" : tone.glyph,
      )}
    >
      <ChevronRight className="size-4" />
    </span>
  );
}

function isDim(status: string): boolean {
  return status === "canceled" || status === "skipped" || status === "failed";
}

// --- status glyph ---

function StatusGlyph({ status, className }: { status: string; className?: string }) {
  const Icon = iconFor(status);
  const cls = cn(
    className,
    status === "running" && "animate-spin",
  );
  return <Icon className={cls} />;
}

function iconFor(status: string): ComponentType<{ className?: string }> {
  switch (status) {
    case "success":
      return CheckCircle2;
    case "failed":
      return XCircle;
    case "running":
      return Loader2;
    case "queued":
      return Clock;
    case "canceled":
      return Ban;
    case "skipped":
      return MinusCircle;
    case "waiting":
    default:
      return CircleDashed;
  }
}

// --- tone palette ---
//
// Every status maps to a design-system token defined in
// app/globals.css → :root (+ .dark). Recoloring the whole
// pipeline-canvas for a brand tweak is a two-line edit in that
// file; this component stays untouched.

type Tone = {
  border: string;
  header: string;
  glyph: string;
  pillBg: string;
  pillBorder: string;
  text: string;
};

const TONE: Record<string, Tone> = {
  success: {
    border: "border-status-success/30",
    header: "bg-status-success-bg text-status-success-fg",
    glyph: "text-status-success",
    pillBg: "bg-status-success-bg",
    pillBorder: "border-status-success/25",
    text: "text-status-success-fg",
  },
  failed: {
    border: "border-status-failed/40",
    header: "bg-status-failed-bg text-status-failed-fg",
    glyph: "text-status-failed",
    pillBg: "bg-status-failed-bg",
    pillBorder: "border-status-failed/30",
    text: "text-status-failed-fg",
  },
  running: {
    border: "border-status-running/40",
    header: "bg-status-running-bg text-status-running-fg",
    glyph: "text-status-running",
    pillBg: "bg-status-running-bg",
    pillBorder: "border-status-running/30",
    text: "text-foreground",
  },
  queued: {
    border: "border-border",
    header: "bg-status-queued-bg text-status-queued-fg",
    glyph: "text-status-queued",
    pillBg: "bg-background",
    pillBorder: "border-border",
    text: "text-foreground",
  },
  canceled: {
    border: "border-border border-dashed",
    header: "bg-status-canceled-bg text-status-canceled-fg",
    glyph: "text-status-canceled",
    pillBg: "bg-status-canceled-bg",
    pillBorder: "border-border border-dashed",
    text: "text-muted-foreground",
  },
  skipped: {
    border: "border-border border-dashed",
    header: "bg-status-skipped-bg text-status-skipped-fg",
    glyph: "text-status-skipped",
    pillBg: "bg-status-skipped-bg",
    pillBorder: "border-border border-dashed",
    text: "text-muted-foreground",
  },
  waiting: {
    border: "border-border",
    header: "bg-background text-muted-foreground",
    glyph: "text-muted-foreground",
    pillBg: "bg-background",
    pillBorder: "border-border",
    text: "text-muted-foreground",
  },
};

function statusTone(status: string): Tone {
  return TONE[status] ?? TONE.waiting!;
}
