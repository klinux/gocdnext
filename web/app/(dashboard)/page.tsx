import Link from "next/link";
import type { Metadata, Route } from "next";
import {
  Activity,
  AlertTriangle,
  ArrowRight,
  CheckCircle2,
  Clock,
  Gauge,
  LayoutDashboard,
  Server,
  Timer,
} from "lucide-react";

import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Separator } from "@/components/ui/separator";
import { RelativeTime } from "@/components/shared/relative-time";
import { StatusBadge } from "@/components/shared/status-badge";
import { StatusDot } from "@/components/shared/status-dot";
import { RunsTable } from "@/components/runs/runs-table";
import { formatDurationSeconds } from "@/lib/format";
import type { StatusTone } from "@/lib/status";
import {
  getDashboardMetrics,
  listAgents,
  listGlobalRunsOnly,
} from "@/server/queries/projects";
import type { AgentSummary } from "@/types/api";

// Maps the backend's AgentHealthState to a StatusTone for the
// design-system components. Stays colocated with the dashboard
// because it's dashboard-specific policy (the agents page may
// pick a slightly different mapping).
function agentHealthTone(health: AgentSummary["health_state"]): StatusTone {
  switch (health) {
    case "online":
      return "success";
    case "stale":
      return "warning";
    case "offline":
      return "failed";
    case "idle":
    default:
      return "neutral";
  }
}

export const metadata: Metadata = {
  title: "Dashboard — gocdnext",
};

export const dynamic = "force-dynamic";

export default async function DashboardPage() {
  // Kick off in parallel so the slowest query sets page TTFB, not
  // the sum. RSC renders only after all three resolve; future work
  // could Suspense-split them if one gets slow.
  const [metrics, runs, agents] = await Promise.all([
    getDashboardMetrics(),
    listGlobalRunsOnly(20),
    listAgents(),
  ]);

  const onlineAgents = agents.filter((a) => a.health_state === "online").length;
  const recentFailures = runs
    .filter((r) => r.status === "failed" || r.status === "canceled")
    .slice(0, 5);

  return (
    <section className="space-y-8">
      <header>
        <h2 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
          <LayoutDashboard className="h-6 w-6" aria-hidden />
          Dashboard
        </h2>
        <p className="mt-1 text-sm text-muted-foreground">
          Activity across every project in this gocdnext instance.
        </p>
      </header>

      <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <Metric
          icon={Activity}
          label="Runs today"
          value={metrics.runs_today.toLocaleString()}
          helper={`${metrics.successes_7d + metrics.failures_7d + metrics.canceled_7d} in last 7d`}
        />
        <Metric
          icon={CheckCircle2}
          label="Success rate"
          value={
            metrics.successes_7d + metrics.failures_7d === 0
              ? "—"
              : `${Math.round(metrics.success_rate_7d * 100)}%`
          }
          helper={`${metrics.successes_7d} ok · ${metrics.failures_7d} failed (7d)`}
          tone={metrics.success_rate_7d >= 0.9 ? "success" : metrics.success_rate_7d >= 0.7 ? "warn" : "danger"}
        />
        <Metric
          icon={Timer}
          label="p50 duration"
          value={
            metrics.p50_seconds_7d > 0
              ? formatDurationSeconds(metrics.p50_seconds_7d)
              : "—"
          }
          helper="median finished run (7d)"
        />
        <Metric
          icon={Gauge}
          label="Queue depth"
          value={metrics.queued_runs.toLocaleString()}
          helper={`${metrics.pending_jobs} job(s) pending`}
          tone={metrics.queued_runs > 0 ? "warn" : "muted"}
        />
      </div>

      <div className="grid gap-6 lg:grid-cols-3">
        <div className="lg:col-span-2 space-y-6">
          <section className="space-y-3">
            <div className="flex items-center justify-between gap-2">
              <h3 className="text-base font-semibold tracking-tight">
                Recent activity
              </h3>
              <Link
                href={"/runs" as Route}
                className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
              >
                All runs <ArrowRight className="h-3 w-3" />
              </Link>
            </div>
            <RunsTable
              runs={runs}
              variant="global"
              emptyMessage="No runs yet. Push a commit or trigger a manual run to get started."
            />
          </section>

          {recentFailures.length > 0 ? (
            <Card>
              <CardHeader>
                <CardTitle className="flex items-center gap-2 text-base">
                  <AlertTriangle className="h-4 w-4 text-destructive" aria-hidden />
                  Needs attention
                </CardTitle>
              </CardHeader>
              <CardContent className="space-y-2">
                {recentFailures.map((r) => (
                  <Link
                    key={r.id}
                    href={`/runs/${r.id}` as Route}
                    className="flex items-center justify-between gap-3 rounded border border-destructive/20 bg-destructive/5 px-3 py-2 text-xs hover:border-destructive/40"
                  >
                    <span className="font-mono truncate">
                      {r.project_slug} / {r.pipeline_name} #{r.counter}
                    </span>
                    <span className="flex items-center gap-2 shrink-0 text-muted-foreground">
                      <RelativeTime at={r.finished_at ?? r.created_at} />
                      <StatusBadge status={r.status} />
                    </span>
                  </Link>
                ))}
              </CardContent>
            </Card>
          ) : null}
        </div>

        <Card>
          <CardHeader className="flex-row items-center justify-between gap-2 space-y-0">
            <CardTitle className="flex items-center gap-2 text-base">
              <Server className="h-4 w-4" aria-hidden />
              Agents
            </CardTitle>
            <span className="text-xs text-muted-foreground">
              {onlineAgents}/{agents.length} online
            </span>
          </CardHeader>
          <CardContent className="px-0 pb-0">
            {agents.length === 0 ? (
              <EmptyState
                icon={Server}
                title="No agents registered"
                body="Deploy the agent chart or run gocdnext-agent locally to pick up jobs."
              />
            ) : (
              <ul className="divide-y divide-border">
                {agents.map((a) => (
                  <AgentRow key={a.id} agent={a} />
                ))}
              </ul>
            )}
          </CardContent>
        </Card>
      </div>
    </section>
  );
}

function Metric({
  icon: Icon,
  label,
  value,
  helper,
  tone = "default",
}: {
  icon: typeof Activity;
  label: string;
  value: string;
  helper?: string;
  tone?: "default" | "success" | "warn" | "danger" | "muted";
}) {
  const toneClass =
    tone === "success"
      ? "text-status-success-fg"
      : tone === "warn"
        ? "text-status-warning-fg"
        : tone === "danger"
          ? "text-status-failed-fg"
          : tone === "muted"
            ? "text-muted-foreground"
            : "text-foreground";
  return (
    <Card>
      <CardContent className="flex items-start justify-between gap-3 p-4">
        <div className="min-w-0">
          <p className="text-[11px] uppercase tracking-wide text-muted-foreground">
            {label}
          </p>
          <p className={`mt-1 text-2xl font-semibold tabular-nums ${toneClass}`}>
            {value}
          </p>
          {helper ? (
            <p className="mt-1 text-[11px] text-muted-foreground">{helper}</p>
          ) : null}
        </div>
        <Icon className="h-4 w-4 text-muted-foreground" aria-hidden />
      </CardContent>
    </Card>
  );
}

function AgentRow({ agent }: { agent: AgentSummary }) {
  return (
    <li className="px-4 py-2.5">
      <div className="flex items-center gap-2">
        <StatusDot tone={agentHealthTone(agent.health_state)} />
        <span className="font-mono text-sm truncate">{agent.name}</span>
        <span className="ml-auto text-xs text-muted-foreground tabular-nums">
          {agent.running_jobs > 0 ? `${agent.running_jobs} running` : "idle"}
        </span>
      </div>
      <div className="mt-0.5 flex items-center gap-2 text-[11px] text-muted-foreground">
        <Clock className="h-3 w-3" aria-hidden />
        <RelativeTime at={agent.last_seen_at} fallback="never" />
        <Separator orientation="vertical" className="mx-1 h-3" />
        <span className="truncate">{agent.tags.join(", ") || "no tags"}</span>
      </div>
    </li>
  );
}

function EmptyState({
  icon: Icon,
  title,
  body,
}: {
  icon: typeof Activity;
  title: string;
  body: string;
}) {
  return (
    <div className="px-4 py-8 text-center">
      <Icon className="mx-auto mb-2 h-5 w-5 text-muted-foreground" aria-hidden />
      <p className="text-sm font-medium">{title}</p>
      <p className="mt-1 text-xs text-muted-foreground">{body}</p>
    </div>
  );
}
