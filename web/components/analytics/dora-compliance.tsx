import { fmtPct } from "@/lib/dora";
import { cn } from "@/lib/utils";
import type { ComplianceCoverageReport, ComplianceGroup } from "@/server/queries/analytics";

// Coverage tone: framework adoption is "more is better".
function coverageTone(rate: number): string {
  if (rate >= 0.8) return "bg-status-success";
  if (rate >= 0.4) return "bg-status-warning";
  return "bg-status-failed";
}

function GroupBlock({ group }: { group: ComplianceGroup }) {
  return (
    <li className="px-3.5 py-3">
      <div className="mb-2 flex items-baseline justify-between">
        <span className="text-sm font-medium">{group.group}</span>
        <span className="text-xs text-muted-foreground">
          {group.projects_total} project{group.projects_total === 1 ? "" : "s"}
        </span>
      </div>
      {group.frameworks.length === 0 ? (
        <p className="text-xs text-muted-foreground">No frameworks bound.</p>
      ) : (
        <div className="space-y-1.5">
          {group.frameworks.map((f) => {
            const rate = group.projects_total > 0 ? f.covered / group.projects_total : 0;
            return (
              <div key={f.framework} className="flex items-center gap-2.5">
                <span className="w-28 shrink-0 truncate text-xs" title={f.framework}>
                  {f.framework}
                </span>
                <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-muted">
                  <div
                    className={cn("h-full rounded-full", coverageTone(rate))}
                    style={{ width: `${Math.round(rate * 100)}%` }}
                  />
                </div>
                <span className="w-20 shrink-0 text-right text-xs tabular-nums text-muted-foreground">
                  {fmtPct(rate)} ({f.covered}/{group.projects_total})
                </span>
              </div>
            );
          })}
        </div>
      )}
    </li>
  );
}

// DoraCompliance is the "compliance posture" section: per label-value group,
// framework adoption (how many of the group's projects are bound to each
// framework). Current state — no window.
export function DoraCompliance({
  report,
  groupKey,
}: {
  report: ComplianceCoverageReport;
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
        <h3 className="text-sm font-semibold">Compliance posture by {groupKey}</h3>
        <p className="text-xs text-muted-foreground">
          Framework adoption across each group&apos;s projects.
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
