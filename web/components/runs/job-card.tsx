import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { StatusBadge } from "@/components/shared/status-badge";
import { RelativeTime } from "@/components/shared/relative-time";
import { LogViewer } from "@/components/runs/log-viewer";
import { cn } from "@/lib/utils";
import { durationBetween, formatDurationSeconds } from "@/lib/format";
import type { JobDetail } from "@/types/api";

type Props = {
  job: JobDetail;
};

export function JobCard({ job }: Props) {
  const duration = formatDurationSeconds(
    durationBetween(job.started_at, job.finished_at),
  );
  const hasLogs = (job.logs?.length ?? 0) > 0;

  return (
    <Card
      id={`job-${job.id}`}
      // :target highlights the card when the URL hash matches this
      // id — the project-page "View logs" dropdown deep-links here
      // and the ring lets the user spot the row after the scroll.
      // CSS-only so no client component needed.
      className={cn(
        "scroll-mt-20 transition-shadow",
        "[&:target]:ring-2 [&:target]:ring-primary [&:target]:ring-offset-2",
      )}
    >
      <CardHeader className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex items-center gap-2">
          <span className="font-mono text-sm font-semibold">{job.name}</span>
          {job.matrix_key ? (
            <span className="font-mono text-xs text-muted-foreground">
              [{job.matrix_key}]
            </span>
          ) : null}
        </div>
        <StatusBadge status={job.status} />
        <div className="ml-auto flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
          {job.image ? <Meta label="image" value={job.image} /> : null}
          {typeof job.exit_code === "number" ? (
            <Meta
              label="exit"
              value={String(job.exit_code)}
              className={
                job.exit_code !== 0 ? "text-destructive font-semibold" : undefined
              }
            />
          ) : null}
          <Meta
            label="started"
            value={<RelativeTime at={job.started_at ?? null} fallback="—" />}
          />
          <Meta label="duration" value={duration} />
        </div>
      </CardHeader>
      <CardContent className="pt-0">
        {job.error ? (
          <p className="mb-2 rounded-md border border-destructive/40 bg-destructive/10 px-3 py-2 text-xs text-destructive">
            {job.error}
          </p>
        ) : null}
        <details open={hasLogs}>
          <summary className="cursor-pointer select-none text-xs text-muted-foreground hover:text-foreground">
            Logs ({job.logs?.length ?? 0})
          </summary>
          <div className="mt-2 overflow-hidden rounded-md border border-border">
            <LogViewer logs={job.logs ?? []} />
          </div>
        </details>
      </CardContent>
    </Card>
  );
}

function Meta({
  label,
  value,
  className,
}: {
  label: string;
  value: React.ReactNode;
  className?: string;
}) {
  return (
    <span className={cn("inline-flex items-center gap-1", className)}>
      <span className="text-[10px] uppercase tracking-wide text-muted-foreground/70">
        {label}
      </span>
      <span className="font-mono">{value}</span>
    </span>
  );
}
