import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { StatusPill } from "@/components/shared/status-pill";
import { severityLabel, severityTone } from "@/lib/severity";
import type { Finding } from "@/types/api";

// FindingsTable renders the security findings list. Presentational (no fetch) so
// it's unit-testable; the page owns the filters + pagination.
export function FindingsTable({ findings }: { findings: Finding[] }) {
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
          </TableRow>
        </TableHeader>
        <TableBody>
          {findings.map((f) => (
            <TableRow key={f.id}>
              <TableCell>
                <StatusPill tone={severityTone(f.severity)}>
                  {severityLabel(f.severity)}
                </StatusPill>
              </TableCell>
              <TableCell>
                <div className="font-mono text-xs font-medium">{f.rule_id}</div>
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
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}
