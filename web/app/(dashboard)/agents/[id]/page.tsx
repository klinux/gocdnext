import Link from "next/link";
import { notFound } from "next/navigation";
import type { Metadata, Route } from "next";
import { Server } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { RelativeTime } from "@/components/shared/relative-time";
import { StatusBadge } from "@/components/shared/status-badge";
import { formatDurationSeconds, durationBetween } from "@/lib/format";
import {
  GocdnextAPIError,
  getAgentDetail,
} from "@/server/queries/projects";
import type { AgentJobSummary, AgentSummary } from "@/types/api";

type Params = { id: string };

export async function generateMetadata({
  params,
}: {
  params: Promise<Params>;
}): Promise<Metadata> {
  const { id } = await params;
  return { title: `Agent ${id.slice(0, 8)} — gocdnext` };
}

export const dynamic = "force-dynamic";

export default async function AgentDetailPage({
  params,
}: {
  params: Promise<Params>;
}) {
  const { id } = await params;
  let data;
  try {
    data = await getAgentDetail(id);
  } catch (err) {
    if (err instanceof GocdnextAPIError && err.status === 404) notFound();
    throw err;
  }
  const { agent, jobs } = data;

  return (
    <section className="space-y-6">
      <header>
        <h2 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
          <Server className="h-6 w-6" aria-hidden />
          <span className="font-mono">{agent.name}</span>
          <HealthBadge state={agent.health_state} />
        </h2>
        <p className="mt-1 text-xs text-muted-foreground font-mono">
          {agent.id}
        </p>
      </header>

      <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <SummaryTile label="Running" value={`${agent.running_jobs} / ${agent.capacity}`} />
        <SummaryTile
          label="Last seen"
          value={agent.last_seen_at ? undefined : "never"}
          valueNode={
            agent.last_seen_at ? (
              <RelativeTime at={agent.last_seen_at} />
            ) : undefined
          }
        />
        <SummaryTile label="Registered" valueNode={<RelativeTime at={agent.registered_at} />} />
        <SummaryTile
          label="Host"
          value={
            agent.os && agent.arch
              ? `${agent.os}/${agent.arch}${agent.version ? " · " + agent.version : ""}`
              : "—"
          }
        />
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Tags</CardTitle>
        </CardHeader>
        <CardContent>
          {agent.tags.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No tags declared. Jobs with required <code>tags:</code> will skip this agent.
            </p>
          ) : (
            <div className="flex flex-wrap gap-1">
              {agent.tags.map((t) => (
                <Badge key={t} variant="secondary">
                  {t}
                </Badge>
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Recent jobs</CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          {jobs.length === 0 ? (
            <p className="px-6 py-8 text-center text-sm text-muted-foreground">
              No jobs dispatched to this agent yet.
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-[120px]">Status</TableHead>
                  <TableHead>Run / job</TableHead>
                  <TableHead className="w-36">Started</TableHead>
                  <TableHead className="w-28">Duration</TableHead>
                  <TableHead className="w-20">Exit</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {jobs.map((j) => (
                  <JobRow key={j.job_run_id} job={j} />
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </section>
  );
}

function SummaryTile({
  label,
  value,
  valueNode,
}: {
  label: string;
  value?: string;
  valueNode?: React.ReactNode;
}) {
  return (
    <Card>
      <CardContent className="p-4">
        <p className="text-[11px] uppercase tracking-wide text-muted-foreground">
          {label}
        </p>
        <p className="mt-1 text-sm font-mono">{valueNode ?? value}</p>
      </CardContent>
    </Card>
  );
}

function JobRow({ job }: { job: AgentJobSummary }) {
  const duration = formatDurationSeconds(
    durationBetween(job.started_at, job.finished_at),
  );
  return (
    <TableRow className="font-mono text-xs">
      <TableCell>
        <StatusBadge status={job.job_status} />
      </TableCell>
      <TableCell className="truncate">
        <Link
          href={`/runs/${job.run_id}` as Route}
          className="hover:underline"
        >
          <span className="text-muted-foreground">
            {job.project_slug}
          </span>{" "}
          / {job.pipeline_name} #{job.run_counter}{" "}
          <span className="text-muted-foreground">/ {job.job_name}</span>
        </Link>
      </TableCell>
      <TableCell>
        <RelativeTime at={job.started_at} fallback="—" />
      </TableCell>
      <TableCell>{duration}</TableCell>
      <TableCell className="tabular-nums">
        {job.exit_code == null ? (
          <span className="text-muted-foreground">—</span>
        ) : (
          job.exit_code
        )}
      </TableCell>
    </TableRow>
  );
}

function HealthBadge({ state }: { state: AgentSummary["health_state"] }) {
  const variant =
    state === "online" ? "success" : state === "stale" ? "secondary" : "outline";
  return (
    <Badge
      variant={variant as "success" | "secondary" | "outline"}
      className="capitalize"
    >
      {state}
    </Badge>
  );
}
