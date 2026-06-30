import { StatusPill } from "@/components/shared/status-pill";
import { SEVERITY_ORDER, severityLabel, severityTone } from "@/lib/severity";
import type { SecurityRollupGroup, SecurityRollupReport } from "@/server/queries/analytics";

function sevCount(g: SecurityRollupGroup, sev: string): number {
  switch (sev) {
    case "critical":
      return g.critical;
    case "high":
      return g.high;
    case "medium":
      return g.medium;
    case "low":
      return g.low;
    default:
      return 0;
  }
}

function GroupBlock({ group }: { group: SecurityRollupGroup }) {
  return (
    <li className="px-3.5 py-3">
      <div className="mb-2 flex items-baseline justify-between gap-2">
        <span className="text-sm font-medium">{group.group}</span>
        <span className="text-xs text-muted-foreground">
          {group.has_scans ? `${group.total_open} open` : "no scans yet"}
        </span>
      </div>
      {!group.has_scans ? null : group.total_open === 0 && group.accepted === 0 ? (
        <p className="text-xs text-muted-foreground">Clean — no open findings.</p>
      ) : (
        <div className="flex flex-wrap items-center gap-1.5">
          {SEVERITY_ORDER.map((sev) => {
            const n = sevCount(group, sev);
            if (n === 0) return null;
            return (
              <StatusPill key={sev} tone={severityTone(sev)}>
                {n} {severityLabel(sev)}
              </StatusPill>
            );
          })}
          {group.accepted > 0 ? (
            <span className="inline-flex items-center rounded-full bg-amber-500/15 px-2.5 py-0.5 text-xs font-medium text-amber-600 dark:text-amber-400">
              {group.accepted} accepted
            </span>
          ) : null}
        </div>
      )}
    </li>
  );
}

// DoraSecurity is the "security posture" section: per label-value group, the open
// vulnerability counts by severity across the group's projects (accepted shown
// separately). Current state — no window. A clean scanned group reads "0 open";
// a never-scanned group is flagged, not dropped.
export function DoraSecurity({
  report,
  groupKey,
}: {
  report: SecurityRollupReport;
  groupKey: string;
}) {
  if (report.groups.length === 0) {
    return (
      <p className="rounded-md border border-dashed p-6 text-center text-sm text-muted-foreground">
        No <span className="font-mono">{groupKey}</span> groups yet.
      </p>
    );
  }
  return (
    <div className="rounded-lg border border-border bg-card">
      <div className="border-b border-border px-3.5 py-2.5">
        <h3 className="text-sm font-semibold">Security findings by {groupKey}</h3>
        <p className="text-xs text-muted-foreground">
          Open vulnerabilities across each group&apos;s projects.
        </p>
      </div>
      <ul className="divide-y divide-border">
        {report.groups.map((g) => (
          <GroupBlock key={g.group} group={g} />
        ))}
      </ul>
    </div>
  );
}
