"use client";

import { useQuery } from "@tanstack/react-query";

import { StatusPill } from "@/components/shared/status-pill";
import { SEVERITY_ORDER, severityLabel, severityTone } from "@/lib/severity";
import { isTerminalStatus } from "@/lib/status";

const SECURITY_POLL_MS = 5_000;

export type RunSecurityFinding = {
  scanner_job: string;
  matrix_key: string;
  tool: string;
  rule_id: string;
  severity: string;
  message: string;
  location_path: string;
  location_line: number;
};

export type RunSecurityData = {
  has_scans: boolean;
  delta_available: boolean;
  unbaselined_series: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  open_total: number;
  accepted: number;
  new_in_change: RunSecurityFinding[];
};

export async function fetchRunSecurity(
  apiBaseURL: string,
  runId: string,
): Promise<RunSecurityData> {
  const res = await fetch(`${apiBaseURL}/api/v1/runs/${runId}/security-findings`, {
    cache: "no-store",
  });
  if (!res.ok) throw new Error(`security: ${res.status}`);
  return (await res.json()) as RunSecurityData;
}

function sevCount(sec: RunSecurityData, sev: string): number {
  switch (sev) {
    case "critical":
      return sec.critical;
    case "high":
      return sec.high;
    case "medium":
      return sec.medium;
    case "low":
      return sec.low;
    default:
      return 0;
  }
}

// RunSecurityPanel shows a run's security posture: open totals by severity
// (identity-deduped) and — for PR runs with a comparable base — the findings
// introduced by this change. "no comparable base" is distinct from "0 new".
export function RunSecurityPanel({
  runId,
  runStatus,
  apiBaseURL,
}: {
  runId: string;
  runStatus: string;
  apiBaseURL: string;
}) {
  const { data, isLoading, error } = useQuery({
    queryKey: ["run-security", runId],
    queryFn: () => fetchRunSecurity(apiBaseURL, runId),
    refetchInterval: isTerminalStatus(runStatus) ? false : SECURITY_POLL_MS,
    staleTime: 30_000,
  });

  if (isLoading) {
    return <p className="text-sm text-muted-foreground">Loading security…</p>;
  }
  if (error) {
    return (
      <p className="text-sm text-destructive">
        Failed to load security findings: {String(error)}
      </p>
    );
  }
  const sec = data;
  if (!sec || !sec.has_scans) {
    return (
      <p className="text-sm text-muted-foreground">
        No security scan reported. Emit a{" "}
        <code className="rounded bg-muted px-1 font-mono text-xs">.sarif</code>{" "}
        artifact from a scanner job (semgrep, trivy, osv-scanner, gitleaks) and
        its findings land here.
      </p>
    );
  }

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        {sec.open_total === 0 ? (
          <span className="text-sm text-muted-foreground">0 open findings</span>
        ) : (
          SEVERITY_ORDER.map((s) => {
            const n = sevCount(sec, s);
            if (n === 0) return null;
            return (
              <StatusPill key={s} tone={severityTone(s)}>
                {n} {severityLabel(s)}
              </StatusPill>
            );
          })
        )}
        {sec.accepted > 0 ? (
          <span className="inline-flex items-center rounded-full bg-amber-500/15 px-2.5 py-0.5 text-xs font-medium text-amber-600 dark:text-amber-400">
            {sec.accepted} accepted
          </span>
        ) : null}
      </div>

      {sec.delta_available ? (
        <div className="rounded-lg border border-border">
          <div className="border-b border-border px-3.5 py-2.5">
            <h3 className="text-sm font-semibold">
              {sec.new_in_change.length} new in this change
            </h3>
            <p className="text-xs text-muted-foreground">
              Findings introduced versus the base branch.
              {sec.unbaselined_series > 0
                ? ` ${sec.unbaselined_series} scanner series have no base to compare against.`
                : ""}
            </p>
          </div>
          {sec.new_in_change.length === 0 ? (
            <p className="px-3.5 py-3 text-sm text-muted-foreground">
              No new findings vs the base branch. 🎉
            </p>
          ) : (
            <ul className="divide-y divide-border">
              {sec.new_in_change.map((f, i) => (
                <li key={`${f.scanner_job}-${f.tool}-${f.rule_id}-${i}`} className="px-3.5 py-2.5">
                  <div className="flex flex-wrap items-center gap-2">
                    <StatusPill tone={severityTone(f.severity)}>
                      {severityLabel(f.severity)}
                    </StatusPill>
                    <span className="font-mono text-xs font-medium">{f.rule_id}</span>
                    <span className="text-xs text-muted-foreground">{f.tool}</span>
                    {f.location_path ? (
                      <span className="font-mono text-xs text-muted-foreground">
                        {f.location_path}
                        {f.location_line ? `:${f.location_line}` : ""}
                      </span>
                    ) : null}
                  </div>
                  {f.message ? (
                    <p className="mt-0.5 line-clamp-2 text-xs text-muted-foreground">{f.message}</p>
                  ) : null}
                </li>
              ))}
            </ul>
          )}
        </div>
      ) : (
        <p className="text-xs text-muted-foreground">
          No comparable base scan — showing current totals only.
          {sec.unbaselined_series > 0
            ? ` ${sec.unbaselined_series} scanner series have no base to compare against.`
            : ""}
        </p>
      )}
    </div>
  );
}
