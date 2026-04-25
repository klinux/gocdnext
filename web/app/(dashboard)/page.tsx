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
  TrendingUp,
} from "lucide-react";

import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Separator } from "@/components/ui/separator";
import { RelativeTime } from "@/components/shared/relative-time";
import { StatusBadge } from "@/components/shared/status-badge";
import { StatusDot } from "@/components/shared/status-dot";
import { RunsTable } from "@/components/runs/runs-table";
import { formatDurationSeconds } from "@/lib/format";
import { cn } from "@/lib/utils";
import type { StatusTone } from "@/lib/status";
import {
  getDashboardMetrics,
  listAgents,
  listGlobalRunsOnly,
} from "@/server/queries/projects";
import type { AgentSummary, GlobalRunSummary } from "@/types/api";

// Maps the backend's AgentHealthState to a StatusTone for the
// design-system components.
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
  const [metrics, runs, agents] = await Promise.all([
    getDashboardMetrics(),
    listGlobalRunsOnly(20),
    listAgents(),
  ]);

  const onlineAgents = agents.filter((a) => a.health_state === "online").length;
  const totalCapacity = agents.reduce((acc, a) => acc + a.capacity, 0);
  const totalRunning = agents.reduce((acc, a) => acc + a.running_jobs, 0);
  const recentFailures = runs
    .filter((r) => r.status === "failed" || r.status === "canceled")
    .slice(0, 5);

  const total7d =
    metrics.successes_7d + metrics.failures_7d + metrics.canceled_7d;
  const successPct = total7d > 0 ? metrics.successes_7d / total7d : 0;
  const failedPct = total7d > 0 ? metrics.failures_7d / total7d : 0;
  const canceledPct = total7d > 0 ? metrics.canceled_7d / total7d : 0;

  const topPipelines = topPipelinesFrom(runs).slice(0, 5);

  return (
    <section className="space-y-6">
      <header>
        <h2 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
          <LayoutDashboard className="h-6 w-6" aria-hidden />
          Dashboard
        </h2>
        <p className="mt-1 text-sm text-muted-foreground">
          Activity across every project on this gocdnext instance.
        </p>
      </header>

      {/* Top metric strip */}
      <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <Metric
          icon={Activity}
          label="Runs today"
          value={metrics.runs_today.toLocaleString()}
          helper={`${total7d.toLocaleString()} in last 7d`}
        />
        <Metric
          icon={CheckCircle2}
          label="Success rate (7d)"
          value={
            total7d === 0
              ? "—"
              : `${Math.round(metrics.success_rate_7d * 100)}%`
          }
          helper={`${metrics.successes_7d.toLocaleString()} ok · ${metrics.failures_7d.toLocaleString()} failed`}
          tone={
            total7d === 0
              ? "muted"
              : metrics.success_rate_7d >= 0.9
                ? "success"
                : metrics.success_rate_7d >= 0.7
                  ? "warn"
                  : "danger"
          }
        />
        <Metric
          icon={Timer}
          label="p50 duration (7d)"
          value={
            metrics.p50_seconds_7d > 0
              ? formatDurationSeconds(metrics.p50_seconds_7d)
              : "—"
          }
          helper="median finished run"
        />
        <Metric
          icon={Gauge}
          label="Queue"
          value={metrics.queued_runs.toLocaleString()}
          helper={`${metrics.pending_jobs} job${metrics.pending_jobs === 1 ? "" : "s"} pending`}
          tone={metrics.queued_runs > 0 ? "warn" : "muted"}
        />
      </div>

      {/* Outcome breakdown + agent capacity row */}
      <div className="grid gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2">
          <CardHeader className="pb-3">
            <CardTitle className="flex items-center gap-2 text-base">
              <TrendingUp className="h-4 w-4 text-muted-foreground" aria-hidden />
              Outcomes (7d)
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            {total7d === 0 ? (
              <p className="py-6 text-center text-sm text-muted-foreground">
                No runs in the last 7 days.
              </p>
            ) : (
              <>
                <div className="flex h-3 overflow-hidden rounded-full bg-muted">
                  <div
                    className="bg-emerald-500 transition-all"
                    style={{ width: `${successPct * 100}%` }}
                    aria-label={`${Math.round(successPct * 100)}% successful`}
                  />
                  <div
                    className="bg-rose-500 transition-all"
                    style={{ width: `${failedPct * 100}%` }}
                    aria-label={`${Math.round(failedPct * 100)}% failed`}
                  />
                  <div
                    className="bg-muted-foreground/40 transition-all"
                    style={{ width: `${canceledPct * 100}%` }}
                    aria-label={`${Math.round(canceledPct * 100)}% canceled`}
                  />
                </div>
                <div className="flex flex-wrap gap-x-6 gap-y-1 text-xs">
                  <Legend
                    color="bg-emerald-500"
                    label="Success"
                    value={metrics.successes_7d}
                  />
                  <Legend
                    color="bg-rose-500"
                    label="Failed"
                    value={metrics.failures_7d}
                  />
                  <Legend
                    color="bg-muted-foreground/40"
                    label="Canceled"
                    value={metrics.canceled_7d}
                  />
                </div>
              </>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="pb-3">
            <CardTitle className="flex items-center gap-2 text-base">
              <Server className="h-4 w-4 text-muted-foreground" aria-hidden />
              Agent capacity
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-3">
            {agents.length === 0 ? (
              <p className="py-6 text-center text-sm text-muted-foreground">
                No agents registered.
              </p>
            ) : (
              <>
                <div className="flex items-baseline justify-between">
                  <span className="text-2xl font-semibold tabular-nums">
                    {totalRunning} / {totalCapacity}
                  </span>
                  <span className="text-xs text-muted-foreground">
                    {onlineAgents}/{agents.length} online
                  </span>
                </div>
                <div className="h-2 overflow-hidden rounded-full bg-muted">
                  <div
                    className={cn(
                      "h-full transition-all",
                      totalCapacity > 0 &&
                        totalRunning / totalCapacity >= 0.8
                        ? "bg-amber-500"
                        : "bg-emerald-500",
                    )}
                    style={{
                      width:
                        totalCapacity > 0
                          ? `${(totalRunning / totalCapacity) * 100}%`
                          : "0%",
                    }}
                    aria-label="Job slot utilization"
                  />
                </div>
                <p className="text-xs text-muted-foreground">
                  Job slots in use across all agents.
                </p>
              </>
            )}
          </CardContent>
        </Card>
      </div>

      {/* Main 2/3 + sidebar 1/3 row */}
      <div className="grid gap-6 lg:grid-cols-3">
        <div className="space-y-6 lg:col-span-2">
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
                  <AlertTriangle
                    className="h-4 w-4 text-destructive"
                    aria-hidden
                  />
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
                    <span className="truncate font-mono">
                      {r.project_slug} / {r.pipeline_name} #{r.counter}
                    </span>
                    <span className="flex shrink-0 items-center gap-2 text-muted-foreground">
                      <RelativeTime at={r.finished_at ?? r.created_at} />
                      <StatusBadge status={r.status} />
                    </span>
                  </Link>
                ))}
              </CardContent>
            </Card>
          ) : null}
        </div>

        <div className="space-y-6">
          {topPipelines.length > 0 ? (
            <Card>
              <CardHeader className="pb-3">
                <CardTitle className="text-base">Most active pipelines</CardTitle>
              </CardHeader>
              <CardContent className="space-y-2">
                {topPipelines.map((p) => (
                  <div
                    key={`${p.project_slug}/${p.pipeline_name}`}
                    className="flex items-center justify-between gap-2 text-xs"
                  >
                    <span className="min-w-0 flex-1 truncate font-mono">
                      <span className="text-muted-foreground">
                        {p.project_slug} /{" "}
                      </span>
                      {p.pipeline_name}
                    </span>
                    <span className="shrink-0 tabular-nums text-muted-foreground">
                      {p.count} run{p.count === 1 ? "" : "s"}
                    </span>
                  </div>
                ))}
              </CardContent>
            </Card>
          ) : null}

          <Card>
            <CardHeader className="flex-row items-center justify-between gap-2 space-y-0">
              <CardTitle className="text-base">Agents</CardTitle>
              <Link
                href={"/agents" as Route}
                className="text-xs text-muted-foreground hover:text-foreground"
              >
                manage
              </Link>
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
                  {agents.slice(0, 6).map((a) => (
                    <AgentRow key={a.id} agent={a} />
                  ))}
                  {agents.length > 6 ? (
                    <li className="px-4 py-2 text-center text-[11px] text-muted-foreground">
                      +{agents.length - 6} more —{" "}
                      <Link
                        href={"/agents" as Route}
                        className="hover:underline"
                      >
                        see all
                      </Link>
                    </li>
                  ) : null}
                </ul>
              )}
            </CardContent>
          </Card>
        </div>
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
      <CardContent className="p-4">
        <div className="flex items-center gap-1.5 text-[11px] uppercase tracking-wide text-muted-foreground">
          <Icon className="h-3.5 w-3.5" aria-hidden />
          {label}
        </div>
        <p className={`mt-1.5 text-2xl font-semibold tabular-nums ${toneClass}`}>
          {value}
        </p>
        {helper ? (
          <p className="mt-1 text-[11px] text-muted-foreground">{helper}</p>
        ) : null}
      </CardContent>
    </Card>
  );
}

function Legend({
  color,
  label,
  value,
}: {
  color: string;
  label: string;
  value: number;
}) {
  return (
    <span className="inline-flex items-center gap-1.5">
      <span className={cn("h-2 w-2 rounded-full", color)} aria-hidden />
      <span className="text-muted-foreground">{label}</span>
      <span className="font-medium tabular-nums">{value.toLocaleString()}</span>
    </span>
  );
}

function AgentRow({ agent }: { agent: AgentSummary }) {
  return (
    <li className="px-4 py-2.5">
      <div className="flex items-center gap-2">
        <StatusDot tone={agentHealthTone(agent.health_state)} />
        <Link
          href={`/agents/${agent.id}` as Route}
          className="truncate font-mono text-sm hover:underline"
        >
          {agent.name}
        </Link>
        <span className="ml-auto text-xs text-muted-foreground tabular-nums">
          {agent.running_jobs > 0
            ? `${agent.running_jobs}/${agent.capacity}`
            : "idle"}
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

// topPipelinesFrom counts pipeline occurrences in a window of
// recent runs so the dashboard can surface the busiest few. The
// window is whatever the page already loads — no extra fetch.
// Limited resolution (last 20 runs only) but gives a useful
// "what's hot right now" answer with zero backend work.
function topPipelinesFrom(runs: GlobalRunSummary[]): Array<{
  project_slug: string;
  pipeline_name: string;
  count: number;
}> {
  const counts = new Map<
    string,
    { project_slug: string; pipeline_name: string; count: number }
  >();
  for (const r of runs) {
    const key = `${r.project_slug}//${r.pipeline_name}`;
    const cur = counts.get(key);
    if (cur) cur.count++;
    else
      counts.set(key, {
        project_slug: r.project_slug,
        pipeline_name: r.pipeline_name,
        count: 1,
      });
  }
  return Array.from(counts.values()).sort((a, b) => b.count - a.count);
}
