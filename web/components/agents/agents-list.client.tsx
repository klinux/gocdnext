"use client";

import { useMemo, useState } from "react";
import Link from "next/link";
import type { Route } from "next";
import { Search, Server } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
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
import { StatusPill } from "@/components/shared/status-pill";
import { cn } from "@/lib/utils";
import type { StatusTone } from "@/lib/status";
import type { AgentSummary } from "@/types/api";

type Props = {
  agents: AgentSummary[];
};

type Health = AgentSummary["health_state"];

const HEALTH_LABELS: Record<Health, string> = {
  online: "Online",
  idle: "Idle",
  stale: "Stale",
  offline: "Offline",
};

// AgentsList renders the global agents page with header stat
// tiles + a name/tag filter + a health-state filter row, on top
// of the previous read-only table. Filtering is client-side
// since the agent list is small (typically <100) and already
// loaded once at page render.
export function AgentsList({ agents }: Props) {
  const [query, setQuery] = useState("");
  const [healthFilter, setHealthFilter] = useState<Health | "all">("all");

  const counts = useMemo(() => {
    const out: Record<Health, number> = {
      online: 0,
      idle: 0,
      stale: 0,
      offline: 0,
    };
    for (const a of agents) out[a.health_state]++;
    return out;
  }, [agents]);

  const totalCapacity = useMemo(
    () => agents.reduce((acc, a) => acc + a.capacity, 0),
    [agents],
  );
  const totalRunning = useMemo(
    () => agents.reduce((acc, a) => acc + a.running_jobs, 0),
    [agents],
  );

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    return agents.filter((a) => {
      if (healthFilter !== "all" && a.health_state !== healthFilter) return false;
      if (!q) return true;
      if (a.name.toLowerCase().includes(q)) return true;
      if (a.tags.some((t) => t.toLowerCase().includes(q))) return true;
      return false;
    });
  }, [agents, query, healthFilter]);

  return (
    <section className="space-y-6">
      <header>
        <h2 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
          <Server className="h-6 w-6" aria-hidden />
          Agents
        </h2>
        <p className="mt-1 text-sm text-muted-foreground">
          {agents.length} agent{agents.length === 1 ? "" : "s"} registered ·{" "}
          {totalRunning}/{totalCapacity} job slots in use
        </p>
      </header>

      {/* Stat strip — clickable health pills double as filters */}
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <StatTile
          label="Online"
          value={counts.online}
          tone="success"
          active={healthFilter === "online"}
          onClick={() =>
            setHealthFilter(healthFilter === "online" ? "all" : "online")
          }
        />
        <StatTile
          label="Idle"
          value={counts.idle}
          tone="running"
          active={healthFilter === "idle"}
          onClick={() =>
            setHealthFilter(healthFilter === "idle" ? "all" : "idle")
          }
        />
        <StatTile
          label="Stale"
          value={counts.stale}
          tone="warning"
          active={healthFilter === "stale"}
          onClick={() =>
            setHealthFilter(healthFilter === "stale" ? "all" : "stale")
          }
        />
        <StatTile
          label="Offline"
          value={counts.offline}
          tone="failed"
          active={healthFilter === "offline"}
          onClick={() =>
            setHealthFilter(healthFilter === "offline" ? "all" : "offline")
          }
        />
      </div>

      {/* Filter input + active filter indicator */}
      <div className="flex items-center justify-between gap-4">
        <div className="relative max-w-sm flex-1">
          <Search
            className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground"
            aria-hidden
          />
          <Input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Filter by name or tag"
            className="pl-8"
          />
        </div>
        {healthFilter !== "all" ? (
          <button
            type="button"
            onClick={() => setHealthFilter("all")}
            className="inline-flex items-center gap-1.5 rounded-md border bg-muted/40 px-2 py-1 text-xs hover:bg-muted"
          >
            health: <span className="font-medium">{HEALTH_LABELS[healthFilter]}</span>
            <span className="text-muted-foreground">✕</span>
          </button>
        ) : null}
        <span className="text-xs text-muted-foreground tabular-nums">
          {filtered.length} of {agents.length}
        </span>
      </div>

      <Card>
        <CardContent className="p-0">
          {agents.length === 0 ? (
            <EmptyState />
          ) : filtered.length === 0 ? (
            <NoMatch />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-[200px]">Agent</TableHead>
                  <TableHead className="w-28">Status</TableHead>
                  <TableHead className="w-44">Slots</TableHead>
                  <TableHead>Tags</TableHead>
                  <TableHead className="w-36">Last seen</TableHead>
                  <TableHead className="w-44">Version</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {filtered.map((a) => (
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

function StatTile({
  label,
  value,
  tone,
  active,
  onClick,
}: {
  label: string;
  value: number;
  tone: StatusTone;
  active: boolean;
  onClick: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "rounded-md border p-3 text-left transition-colors hover:bg-muted/40",
        active && "border-primary/60 bg-primary/5",
      )}
    >
      <div className="flex items-center justify-between gap-2">
        <span className="text-xs text-muted-foreground">{label}</span>
        <StatusDot tone={tone} label={label} />
      </div>
      <div className="mt-1 text-2xl font-semibold tabular-nums">{value}</div>
    </button>
  );
}

function AgentRow({ agent }: { agent: AgentSummary }) {
  const utilization =
    agent.capacity > 0 ? agent.running_jobs / agent.capacity : 0;
  const utilPct = Math.round(utilization * 100);

  return (
    <TableRow>
      <TableCell className="font-mono text-xs">
        <Link
          href={`/agents/${agent.id}` as Route}
          className="inline-flex items-center gap-2 hover:underline"
        >
          <StatusDot
            tone={agentHealthTone(agent.health_state)}
            label={agent.health_state}
          />
          <span>{agent.name}</span>
        </Link>
      </TableCell>
      <TableCell>
        <StatusPill tone={agentHealthTone(agent.health_state)}>
          {HEALTH_LABELS[agent.health_state]}
        </StatusPill>
      </TableCell>
      <TableCell>
        <div className="flex items-center gap-2">
          <span className="font-mono text-xs tabular-nums">
            {agent.running_jobs}/{agent.capacity}
          </span>
          <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-muted">
            <div
              className={cn(
                "h-full transition-all",
                utilPct === 0 && "bg-muted-foreground/30",
                utilPct > 0 && utilPct < 80 && "bg-emerald-500",
                utilPct >= 80 && utilPct < 100 && "bg-amber-500",
                utilPct >= 100 && "bg-rose-500",
              )}
              style={{ width: `${Math.min(100, utilPct)}%` }}
              aria-label={`${utilPct}% utilized`}
            />
          </div>
        </div>
      </TableCell>
      <TableCell>
        {agent.tags.length === 0 ? (
          <span className="text-xs text-muted-foreground">—</span>
        ) : (
          <span className="flex flex-wrap gap-1">
            {agent.tags.map((t) => (
              <Badge
                key={t}
                variant="secondary"
                className="font-mono text-[10px]"
              >
                {t}
              </Badge>
            ))}
          </span>
        )}
      </TableCell>
      <TableCell className="text-xs text-muted-foreground">
        <RelativeTime at={agent.last_seen_at} fallback="never" />
      </TableCell>
      <TableCell className="truncate text-xs text-muted-foreground">
        {agent.version ? agent.version : "—"}
        {agent.os ? ` · ${agent.os}/${agent.arch}` : ""}
      </TableCell>
    </TableRow>
  );
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

function EmptyState() {
  return (
    <div className="py-16 text-center">
      <Server
        className="mx-auto size-8 text-muted-foreground/60"
        aria-hidden
      />
      <p className="mt-3 text-sm font-medium">No agents registered</p>
      <p className="mt-1 text-xs text-muted-foreground">
        Deploy the agent Helm chart or run{" "}
        <code className="rounded bg-muted px-1 py-0.5">
          gocdnext-agent
        </code>{" "}
        pointing at{" "}
        <code className="rounded bg-muted px-1 py-0.5">:8154</code>.
      </p>
    </div>
  );
}

function NoMatch() {
  return (
    <div className="py-12 text-center text-sm text-muted-foreground">
      No agents match the current filters.
    </div>
  );
}
