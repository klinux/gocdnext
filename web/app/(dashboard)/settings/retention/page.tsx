import type { Metadata } from "next";

import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { getRetentionSnapshot } from "@/server/queries/admin";

export const metadata: Metadata = {
  title: "Settings — Retention",
};

export default async function RetentionPage() {
  const snap = await getRetentionSnapshot();

  if (!snap.enabled) {
    return (
      <Card>
        <CardHeader>
          <CardTitle>Retention disabled</CardTitle>
          <CardDescription>
            No artifact backend is configured on this server, so the sweeper
            is inactive. Set <code className="font-mono">GOCDNEXT_ARTIFACTS_*</code>{" "}
            env vars and restart to enable automatic cleanup.
          </CardDescription>
        </CardHeader>
      </Card>
    );
  }

  const last = snap.last_stats;
  const lastAt = snap.last_sweep_at ? fmtAt(snap.last_sweep_at) : "never";

  return (
    <div className="grid gap-4 md:grid-cols-2">
      <Card>
        <CardHeader>
          <CardTitle>Policy</CardTitle>
          <CardDescription>Configured at server boot (env vars).</CardDescription>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-2 gap-y-2 text-sm">
            <Row k="Tick">{fmtDuration(snap.tick)}</Row>
            <Row k="Batch size">{snap.batch_size}</Row>
            <Row k="Retry grace">{snap.grace_minutes} min</Row>
            <Row k="Keep last N">{snap.keep_last || "off"}</Row>
            <Row k="Project quota">{fmtBytes(snap.project_quota_bytes)}</Row>
            <Row k="Global quota">{fmtBytes(snap.global_quota_bytes)}</Row>
          </dl>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Last sweep</CardTitle>
          <CardDescription>{lastAt}</CardDescription>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-2 gap-y-2 text-sm">
            <Row k="Claimed">{last.Claimed}</Row>
            <Row k="Deleted">{last.Deleted}</Row>
            <Row k="Bytes freed">{fmtBytes(last.BytesFreed)}</Row>
            <Row k="Storage failures">
              {last.StorageFailures > 0 ? (
                <Badge variant="destructive">{last.StorageFailures}</Badge>
              ) : (
                last.StorageFailures
              )}
            </Row>
            <Row k="DB failures">
              {last.DBFailures > 0 ? (
                <Badge variant="destructive">{last.DBFailures}</Badge>
              ) : (
                last.DBFailures
              )}
            </Row>
            <Row k="Demoted (keep-last)">{last.DemotedKeepLast}</Row>
            <Row k="Demoted (project cap)">{last.DemotedProjectCap}</Row>
            <Row k="Demoted (global cap)">{last.DemotedGlobalCap}</Row>
          </dl>
        </CardContent>
      </Card>
    </div>
  );
}

function Row({ k, children }: { k: string; children: React.ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground">{k}</dt>
      <dd className="font-medium">{children}</dd>
    </>
  );
}

function fmtBytes(n: number) {
  if (n === 0) return "off";
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

function fmtAt(at: string) {
  try {
    return new Date(at).toLocaleString();
  } catch {
    return at;
  }
}
