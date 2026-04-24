import type { Metadata } from "next";
import type { ReactNode } from "react";
import {
  AlertTriangle,
  Archive,
  CheckCircle2,
  Database,
  HardDrive,
  Layers,
  Lock,
  Timer,
  Trash2,
} from "lucide-react";

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { StatusPill } from "@/components/shared/status-pill";
import { getRetentionSnapshot } from "@/server/queries/admin";

export const metadata: Metadata = {
  title: "Settings — Retention",
};

// force-dynamic so last-sweep stats refresh on every view —
// the sweeper runs on its own tick and the admin panel is the
// only place these numbers surface.
export const dynamic = "force-dynamic";

export default async function RetentionPage() {
  const snap = await getRetentionSnapshot();

  if (!snap.enabled) {
    return (
      <Card className="border-amber-500/40 bg-amber-500/5">
        <CardHeader className="flex-row items-start gap-3 space-y-0">
          <AlertTriangle className="mt-0.5 size-5 shrink-0 text-amber-500" />
          <div>
            <CardTitle className="text-base">Retention sweeper is off</CardTitle>
            <CardDescription className="mt-1">
              No artifact backend is configured on this server, so the sweeper
              is inactive. Set{" "}
              <code className="rounded bg-muted px-1 font-mono text-xs">
                GOCDNEXT_ARTIFACTS_*
              </code>{" "}
              env vars and restart to enable automatic cleanup.
            </CardDescription>
          </div>
        </CardHeader>
      </Card>
    );
  }

  const last = snap.last_stats;
  const hasFailures = last.StorageFailures > 0 || last.DBFailures > 0;
  const lastAt = snap.last_sweep_at ? fmtRelative(snap.last_sweep_at) : null;

  return (
    <div className="space-y-4">
      {/* Lock badge + read-only note sets expectations early */}
      <div className="flex items-center gap-2 rounded-md border border-border bg-muted/30 px-3 py-2 text-xs text-muted-foreground">
        <Lock className="size-3.5 shrink-0" aria-hidden />
        <span>
          All values below come from{" "}
          <code className="rounded bg-muted px-1 font-mono">
            GOCDNEXT_ARTIFACTS_*
          </code>{" "}
          and{" "}
          <code className="rounded bg-muted px-1 font-mono">
            GOCDNEXT_CACHE_*
          </code>{" "}
          env vars. Restart the server to apply changes.
        </span>
      </div>

      {/* Hero: last sweep summary */}
      <Card>
        <CardHeader className="flex-row items-start justify-between space-y-0">
          <div>
            <CardTitle className="text-base">Last sweep</CardTitle>
            <CardDescription>
              {lastAt ? `Ran ${lastAt}.` : "Never fired — waiting for the first tick."}
            </CardDescription>
          </div>
          {hasFailures ? (
            <StatusPill tone="failed" icon={AlertTriangle}>
              {last.StorageFailures + last.DBFailures} failures
            </StatusPill>
          ) : lastAt ? (
            <StatusPill tone="success" icon={CheckCircle2}>
              clean
            </StatusPill>
          ) : (
            <StatusPill tone="neutral">idle</StatusPill>
          )}
        </CardHeader>
        <CardContent className="grid grid-cols-2 gap-3 md:grid-cols-4">
          <Stat
            icon={Archive}
            label="Claimed"
            value={last.Claimed.toLocaleString()}
            hint="rows picked this tick"
          />
          <Stat
            icon={Trash2}
            label="Deleted"
            value={last.Deleted.toLocaleString()}
            hint="physically removed"
          />
          <Stat
            icon={HardDrive}
            label="Freed"
            value={fmtBytes(last.BytesFreed)}
            hint="storage reclaimed"
          />
          <Stat
            icon={Layers}
            label="Demoted"
            value={(
              last.DemotedKeepLast +
              last.DemotedProjectCap +
              last.DemotedGlobalCap
            ).toLocaleString()}
            hint="marked for later sweep"
            breakdown={
              <>
                keep-last: {last.DemotedKeepLast} · project-cap:{" "}
                {last.DemotedProjectCap} · global-cap:{" "}
                {last.DemotedGlobalCap}
              </>
            }
          />
        </CardContent>
      </Card>

      {/* Policy + failures split */}
      <div className="grid gap-4 md:grid-cols-2">
        <Card>
          <CardHeader>
            <div className="flex items-center gap-2">
              <Database className="size-4 text-muted-foreground" aria-hidden />
              <CardTitle className="text-base">Artifact policy</CardTitle>
            </div>
            <CardDescription>
              When artifacts get demoted to reclaim, and how much storage is
              allowed per scope.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-2.5">
            <PolicyRow
              label="Keep last"
              env="GOCDNEXT_ARTIFACTS_KEEP_LAST"
              value={snap.keep_last > 0 ? `${snap.keep_last} runs` : "off"}
              hint="Always keep the N latest runs per pipeline."
            />
            <PolicyRow
              label="Project quota"
              env="GOCDNEXT_ARTIFACTS_PROJECT_QUOTA_BYTES"
              value={fmtBytes(snap.project_quota_bytes)}
              hint="Cap on artifact bytes per project."
            />
            <PolicyRow
              label="Global quota"
              env="GOCDNEXT_ARTIFACTS_GLOBAL_QUOTA_BYTES"
              value={fmtBytes(snap.global_quota_bytes)}
              hint="Hard ceiling across all projects."
            />
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <div className="flex items-center gap-2">
              <Timer className="size-4 text-muted-foreground" aria-hidden />
              <CardTitle className="text-base">Sweeper engine</CardTitle>
            </div>
            <CardDescription>
              Cadence + batch sizing for the background reclaim loop.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-2.5">
            <PolicyRow
              label="Tick"
              env="GOCDNEXT_RETENTION_TICK"
              value={fmtDuration(snap.tick)}
              hint="How often the sweeper wakes up."
            />
            <PolicyRow
              label="Batch size"
              env="GOCDNEXT_RETENTION_BATCH"
              value={snap.batch_size.toLocaleString()}
              hint="Max rows claimed per tick."
            />
            <PolicyRow
              label="Retry grace"
              env="GOCDNEXT_RETENTION_GRACE_MINUTES"
              value={`${snap.grace_minutes} min`}
              hint="Wait before retrying storage failures."
            />
          </CardContent>
        </Card>
      </div>

      {/* Failures drawer — only rendered when something failed last run */}
      {hasFailures ? (
        <Card className="border-destructive/40">
          <CardHeader>
            <div className="flex items-center gap-2">
              <AlertTriangle className="size-4 text-destructive" aria-hidden />
              <CardTitle className="text-base">Failures in last sweep</CardTitle>
            </div>
            <CardDescription>
              The sweeper will retry these rows after the{" "}
              {snap.grace_minutes}-minute grace window.
            </CardDescription>
          </CardHeader>
          <CardContent className="grid grid-cols-2 gap-3">
            <Stat
              icon={HardDrive}
              label="Storage failures"
              value={last.StorageFailures.toLocaleString()}
              hint="provider delete errors"
              tone="failed"
            />
            <Stat
              icon={Database}
              label="DB failures"
              value={last.DBFailures.toLocaleString()}
              hint="inconsistent state in postgres"
              tone="failed"
            />
          </CardContent>
        </Card>
      ) : null}
    </div>
  );
}

function Stat({
  icon: Icon,
  label,
  value,
  hint,
  breakdown,
  tone,
}: {
  icon: React.ComponentType<{ className?: string }>;
  label: string;
  value: string;
  hint?: string;
  breakdown?: ReactNode;
  tone?: "failed";
}) {
  return (
    <div
      className={`rounded-md border p-3 ${
        tone === "failed" ? "border-destructive/40 bg-destructive/5" : "bg-muted/20"
      }`}
    >
      <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
        <Icon className="size-3.5" aria-hidden />
        {label}
      </div>
      <div
        className={`mt-1 text-2xl font-semibold tabular-nums ${
          tone === "failed" ? "text-destructive" : ""
        }`}
      >
        {value}
      </div>
      {hint ? (
        <div className="mt-0.5 text-[11px] text-muted-foreground">{hint}</div>
      ) : null}
      {breakdown ? (
        <div className="mt-2 text-[11px] text-muted-foreground">
          {breakdown}
        </div>
      ) : null}
    </div>
  );
}

function PolicyRow({
  label,
  env,
  value,
  hint,
}: {
  label: string;
  env: string;
  value: string;
  hint?: string;
}) {
  return (
    <div className="flex items-start justify-between gap-4">
      <div className="min-w-0 flex-1">
        <div className="flex items-baseline gap-2">
          <p className="text-sm font-medium">{label}</p>
          <code className="truncate text-[10px] text-muted-foreground">
            {env}
          </code>
        </div>
        {hint ? (
          <p className="mt-0.5 text-xs text-muted-foreground">{hint}</p>
        ) : null}
      </div>
      <div className="shrink-0 text-sm font-semibold tabular-nums">{value}</div>
    </div>
  );
}

function fmtBytes(n: number) {
  if (n === 0) return "unlimited";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(v >= 10 || i === 0 ? 0 : 1)} ${units[i]}`;
}

// Sweeper Snapshot.tick is reported in nanoseconds (Go time.Duration
// marshalled as number). Convert to a human-readable interval.
function fmtDuration(ns: number) {
  const seconds = Math.round(ns / 1e9);
  if (seconds < 60) return `${seconds}s`;
  const m = Math.round(seconds / 60);
  if (m < 60) return `${m} min`;
  return `${(m / 60).toFixed(1)} h`;
}

function fmtRelative(at: string) {
  try {
    const then = new Date(at).getTime();
    const diff = Date.now() - then;
    const mins = Math.round(diff / 60000);
    if (mins < 1) return "just now";
    if (mins < 60) return `${mins}m ago`;
    const hrs = Math.round(mins / 60);
    if (hrs < 24) return `${hrs}h ago`;
    const days = Math.round(hrs / 24);
    return `${days}d ago`;
  } catch {
    return at;
  }
}
