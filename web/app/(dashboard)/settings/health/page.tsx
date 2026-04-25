import type { Metadata } from "next";
import {
  Activity,
  AlertTriangle,
  CheckCircle2,
  Database,
  HelpCircle,
  Server,
  Timer,
} from "lucide-react";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { StatusPill } from "@/components/shared/status-pill";
import { getAdminHealth } from "@/server/queries/admin";
import { cn } from "@/lib/utils";
import type { StatusTone } from "@/lib/status";
import type { AdminHealth } from "@/types/api";

export const metadata: Metadata = {
  title: "Settings — Health",
};

export const dynamic = "force-dynamic";

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
          <CardTitle className="flex items-center gap-2">
            <AlertTriangle className="size-4 text-destructive" aria-hidden />
            Failed to load health
          </CardTitle>
          <CardDescription>{error ?? "Unknown error"}</CardDescription>
        </CardHeader>
      </Card>
    );
  }

  const successPct = Math.round((health.success_rate_7d ?? 0) * 100);
  const overallTone: StatusTone =
    !health.db_ok || health.agents_offline > 0
      ? "failed"
      : health.agents_stale > 0 || successPct < 70 || health.queued_runs > 20
        ? "warning"
        : "success";
  const overallLabel =
    overallTone === "success"
      ? "All systems nominal"
      : overallTone === "warning"
        ? "Watch — degraded signals"
        : "Alert — needs attention";

  return (
    <div className="space-y-4">
      {/* Hero status banner */}
      <Card
        className={cn(
          "border-l-4",
          overallTone === "success" && "border-l-emerald-500",
          overallTone === "warning" && "border-l-amber-500",
          overallTone === "failed" && "border-l-rose-500",
        )}
      >
        <CardHeader className="flex-row items-center justify-between space-y-0">
          <div>
            <CardTitle className="text-base">{overallLabel}</CardTitle>
            <CardDescription className="mt-0.5">
              Checked {fmtAt(health.checked_at)}
            </CardDescription>
          </div>
          <StatusPill tone={overallTone}>
            {overallTone === "success" ? "Healthy" : overallTone === "warning" ? "Watch" : "Alert"}
          </StatusPill>
        </CardHeader>
      </Card>

      {/* Tile grid */}
      <div className="grid gap-3 md:grid-cols-2 lg:grid-cols-3">
        <Tile
          icon={Database}
          label="Database"
          value={health.db_ok ? "Reachable" : "Unreachable"}
          hint={
            health.db_error ||
            (health.db_ok
              ? "Postgres responded to the latest probe."
              : "Last probe failed.")
          }
          tone={health.db_ok ? "success" : "failed"}
        />
        <Tile
          icon={Server}
          label="Agents"
          value={`${health.agents_online} online`}
          hint={`${health.agents_stale} stale · ${health.agents_offline} offline`}
          tone={
            health.agents_online === 0
              ? "warning"
              : health.agents_offline > 0
                ? "failed"
                : "success"
          }
        />
        <Tile
          icon={Activity}
          label="Queue depth"
          value={health.queued_runs.toLocaleString()}
          hint={`${health.pending_jobs.toLocaleString()} job${
            health.pending_jobs === 1 ? "" : "s"
          } pending`}
          tone={
            health.queued_runs > 20
              ? "warning"
              : health.queued_runs > 0
                ? "running"
                : "success"
          }
        />
        <Tile
          icon={CheckCircle2}
          label="Success rate (7d)"
          value={`${successPct}%`}
          hint="across every project's runs"
          tone={successPct >= 80 ? "success" : successPct >= 50 ? "warning" : "failed"}
        />
        <Tile
          icon={Timer}
          label="Last check"
          value={fmtAt(health.checked_at)}
          hint="auto-refreshed when this page reloads"
          tone="neutral"
        />
        <Card className="border-dashed">
          <CardHeader className="pb-2">
            <div className="flex items-center gap-2">
              <HelpCircle className="size-4 text-muted-foreground" aria-hidden />
              <CardTitle className="text-sm font-medium text-muted-foreground">
                What's checked
              </CardTitle>
            </div>
          </CardHeader>
          <CardContent className="text-xs text-muted-foreground">
            DB connectivity, agent health (online/stale/offline), queue depth,
            and 7-day run outcomes. Set up Prometheus scraping at{" "}
            <code className="rounded bg-muted px-1">/metrics</code> for richer
            dashboards.
          </CardContent>
        </Card>
      </div>
    </div>
  );
}

function Tile({
  icon: Icon,
  label,
  value,
  hint,
  tone,
}: {
  icon: React.ComponentType<{ className?: string }>;
  label: string;
  value: string;
  hint?: string;
  tone: StatusTone;
}) {
  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
        <div className="flex items-center gap-2">
          <Icon className="size-4 text-muted-foreground" aria-hidden />
          <CardTitle className="text-sm font-medium text-muted-foreground">
            {label}
          </CardTitle>
        </div>
        <StatusPill tone={tone}>
          {tone === "success"
            ? "ok"
            : tone === "warning"
              ? "watch"
              : tone === "failed"
                ? "alert"
                : tone === "running"
                  ? "active"
                  : "info"}
        </StatusPill>
      </CardHeader>
      <CardContent>
        <div className="text-2xl font-semibold tabular-nums">{value}</div>
        {hint ? (
          <p className="mt-1 text-xs text-muted-foreground">{hint}</p>
        ) : null}
      </CardContent>
    </Card>
  );
}

function fmtAt(at: string) {
  try {
    return new Date(at).toLocaleString();
  } catch {
    return at;
  }
}
