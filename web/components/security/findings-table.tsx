import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { StatusPill } from "@/components/shared/status-pill";
import { FindingStateMenu } from "@/components/security/finding-state-menu.client";
import { cn } from "@/lib/utils";
import { severityLabel, severityTone } from "@/lib/severity";
import type { Finding } from "@/types/api";

// FindingsTable renders the security findings list. Presentational (no fetch) so
// it's unit-testable; the page owns the filters + pagination. The per-row state
// menu is the one interactive island (a client component).
export function FindingsTable({ findings, slug }: { findings: Finding[]; slug: string }) {
  return (
    <div className="rounded-lg border border-border">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="w-24">Severity</TableHead>
            <TableHead>Rule</TableHead>
            <TableHead>Tool</TableHead>
            <TableHead>Location</TableHead>
            <TableHead>Pipeline · job</TableHead>
            <TableHead className="w-40">State</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {findings.map((f) => (
            <TableRow
              key={f.id}
              className={cn(
                f.state === "dismissed" || f.state === "false_positive" ? "opacity-55" : undefined,
              )}
            >
              <TableCell>
                <StatusPill tone={severityTone(f.severity)}>
                  {severityLabel(f.severity)}
                </StatusPill>
              </TableCell>
              <TableCell>
                <div className="flex items-center gap-1.5">
                  <span className="font-mono text-xs font-medium">{f.rule_id}</span>
                  {f.status === "new" ? (
                    <span className="rounded-full bg-primary/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase leading-none tracking-wide text-primary">
                      New
                    </span>
                  ) : null}
                </div>
                {f.message ? (
                  <div className="line-clamp-2 max-w-[420px] text-xs text-muted-foreground">
                    {f.message}
                  </div>
                ) : null}
              </TableCell>
              <TableCell className="text-xs">{f.tool}</TableCell>
              <TableCell className="font-mono text-xs text-muted-foreground">
                {f.location_path
                  ? `${f.location_path}${f.location_line ? `:${f.location_line}` : ""}`
                  : "—"}
              </TableCell>
              <TableCell className="text-xs text-muted-foreground">{f.job_name}</TableCell>
              <TableCell>
                <FindingStateMenu slug={slug} stateId={f.state_id} state={f.state} />
                {f.state_reason ? (
                  <div className="mt-0.5 max-w-[200px] truncate text-[10px] text-muted-foreground" title={f.state_reason}>
                    {f.state_reason}
                  </div>
                ) : null}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}
