import type { Metadata } from "next";
import { Activity, CheckCircle2, Database, Server, Timer } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { getAdminHealth } from "@/server/queries/admin";
import type { AdminHealth } from "@/types/api";

export const metadata: Metadata = {
  title: "Settings — Health",
};

export default async function HealthPage() {
  let health: AdminHealth | null = null;
  let error: string | null = null;
  try {
    health = await getAdminHealth();
  } catch (e) {
    error = e instanceof Error ? e.message : String(e);
  }

  if (error || !health) {
    return (
      <Card className="border-destructive/50">
        <CardHeader>
          <CardTitle>Failed to load health</CardTitle>
          <CardDescription>{error ?? "Unknown error"}</CardDescription>
        </CardHeader>
      </Card>
    );
  }

  const pct = Math.round((health.success_rate_7d ?? 0) * 100);

  return (
    <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
      <Tile
        icon={Database}
        label="Database"
        value={health.db_ok ? "Reachable" : "Unreachable"}
        tone={health.db_ok ? "ok" : "bad"}
        hint={health.db_error || `checked ${fmtAt(health.checked_at)}`}
      />
      <Tile
        icon={Server}
        label="Agents"
        value={`${health.agents_online} online`}
        tone={health.agents_online > 0 ? "ok" : "warn"}
        hint={`${health.agents_stale} stale · ${health.agents_offline} offline`}
      />
      <Tile
        icon={Activity}
        label="Queue"
        value={`${health.queued_runs} queued`}
        tone={health.queued_runs > 20 ? "warn" : "ok"}
        hint={`${health.pending_jobs} jobs pending`}
      />
      <Tile
        icon={CheckCircle2}
        label="Success rate (7d)"
        value={`${pct}%`}
        tone={pct >= 80 ? "ok" : pct >= 50 ? "warn" : "bad"}
      />
      <Tile
        icon={Timer}
        label="Last check"
        value={fmtAt(health.checked_at)}
        tone="muted"
      />
    </div>
  );
}

type Tone = "ok" | "warn" | "bad" | "muted";

function Tile({
  icon: Icon,
  label,
  value,
  tone,
  hint,
}: {
  icon: React.ComponentType<{ className?: string }>;
  label: string;
  value: string;
  tone: Tone;
  hint?: string;
}) {
  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between pb-2 space-y-0">
        <CardTitle className="text-sm font-medium text-muted-foreground">
          {label}
        </CardTitle>
        <Icon className="size-4 text-muted-foreground" aria-hidden />
      </CardHeader>
      <CardContent className="space-y-2">
        <div className="text-2xl font-semibold">{value}</div>
        <div className="flex items-center gap-2">
          <ToneBadge tone={tone} />
          {hint ? <span className="text-xs text-muted-foreground truncate">{hint}</span> : null}
        </div>
      </CardContent>
    </Card>
  );
}

function ToneBadge({ tone }: { tone: Tone }) {
  if (tone === "ok") return <Badge variant="secondary">ok</Badge>;
  if (tone === "warn") return <Badge variant="outline">watch</Badge>;
  if (tone === "bad") return <Badge variant="destructive">alert</Badge>;
  return <Badge variant="outline">info</Badge>;
}

function fmtAt(at: string) {
  try {
    return new Date(at).toLocaleString();
  } catch {
    return at;
  }
}
