import { StatusBadge } from "@/components/shared/status-badge";
import { RelativeTime } from "@/components/shared/relative-time";
import { JobCard } from "@/components/runs/job-card";
import { durationBetween, formatDurationSeconds } from "@/lib/format";
import type { StageDetail } from "@/types/api";

type Props = {
  stage: StageDetail;
};

export function StageSection({ stage }: Props) {
  const duration = formatDurationSeconds(
    durationBetween(stage.started_at, stage.finished_at),
  );
  return (
    <section aria-labelledby={`stage-${stage.id}`}>
      <header className="flex flex-wrap items-center gap-3 border-b border-border pb-2 mb-3">
        <span className="text-xs text-muted-foreground">
          #{stage.ordinal + 1}
        </span>
        <h3
          id={`stage-${stage.id}`}
          className="text-base font-semibold tracking-tight"
        >
          {stage.name}
        </h3>
        <StatusBadge status={stage.status} />
        <div className="ml-auto flex items-center gap-3 text-xs text-muted-foreground">
          <span>
            started <RelativeTime at={stage.started_at ?? null} fallback="—" />
          </span>
          <span>{duration}</span>
        </div>
      </header>
      <div className="space-y-3">
        {stage.jobs.map((j) => (
          <JobCard key={j.id} job={j} />
        ))}
      </div>
    </section>
  );
}
