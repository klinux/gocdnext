import Link from "next/link";
import type { Metadata, Route } from "next";
import { Server } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Card, CardContent } from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { RelativeTime } from "@/components/shared/relative-time";
import { StatusDot } from "@/components/shared/status-dot";
import type { StatusTone } from "@/lib/status";
import { listAgents } from "@/server/queries/projects";
import type { AgentSummary } from "@/types/api";

export const metadata: Metadata = {
  title: "Agents — gocdnext",
};

export const dynamic = "force-dynamic";

export default async function AgentsListPage() {
  const agents = await listAgents();
  const online = agents.filter((a) => a.health_state === "online").length;

  return (
    <section className="space-y-6">
      <header>
        <h2 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
          <Server className="h-6 w-6" aria-hidden />
          Agents
        </h2>
        <p className="mt-1 text-sm text-muted-foreground">
          {online} of {agents.length} online.
        </p>
      </header>

      <Card>
        <CardContent className="p-0">
          {agents.length === 0 ? (
            <div className="py-16 text-center text-sm text-muted-foreground">
              No agents registered. Deploy the agent Helm chart or run
              <code className="mx-1 rounded bg-muted px-1 py-0.5">
                gocdnext-agent
              </code>
              pointing at{" "}
              <code className="rounded bg-muted px-1 py-0.5">:8154</code>.
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-[180px]">Agent</TableHead>
                  <TableHead className="w-24">Status</TableHead>
                  <TableHead className="w-28">Running / cap</TableHead>
                  <TableHead>Tags</TableHead>
                  <TableHead className="w-36">Last seen</TableHead>
                  <TableHead className="w-40">Version</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {agents.map((a) => (
                  <AgentRow key={a.id} agent={a} />
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </section>
  );
}

function AgentRow({ agent }: { agent: AgentSummary }) {
  return (
    <TableRow className="font-mono text-xs">
      <TableCell>
        <Link
          href={`/agents/${agent.id}` as Route}
          className="inline-flex items-center gap-2 hover:underline"
        >
          <HealthDot state={agent.health_state} />
          <span>{agent.name}</span>
        </Link>
      </TableCell>
      <TableCell>
        <HealthBadge state={agent.health_state} />
      </TableCell>
      <TableCell className="tabular-nums">
        {agent.running_jobs} / {agent.capacity}
      </TableCell>
      <TableCell className="truncate">
        {agent.tags.length === 0 ? (
          <span className="text-muted-foreground">—</span>
        ) : (
          <span className="flex flex-wrap gap-1">
            {agent.tags.map((t) => (
              <Badge key={t} variant="secondary" className="text-[10px]">
                {t}
              </Badge>
            ))}
          </span>
        )}
      </TableCell>
      <TableCell>
        <RelativeTime at={agent.last_seen_at} fallback="never" />
      </TableCell>
      <TableCell className="text-muted-foreground truncate">
        {agent.version ? `${agent.version}` : "—"}
        {agent.os ? ` · ${agent.os}/${agent.arch}` : ""}
      </TableCell>
    </TableRow>
  );
}

function HealthDot({ state }: { state: AgentSummary["health_state"] }) {
  return <StatusDot tone={agentHealthTone(state)} label={state} />;
}

function agentHealthTone(state: AgentSummary["health_state"]): StatusTone {
  switch (state) {
    case "online":
      return "success";
    case "stale":
      return "warning";
    case "idle":
      return "running";
    case "offline":
    default:
      return "failed";
  }
}

function HealthBadge({ state }: { state: AgentSummary["health_state"] }) {
  const variant =
    state === "online" ? "success" : state === "stale" ? "secondary" : "outline";
  return (
    <Badge variant={variant as "success" | "secondary" | "outline"} className="capitalize">
      {state}
    </Badge>
  );
}
