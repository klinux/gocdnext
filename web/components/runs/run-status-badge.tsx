import type { Route } from "next";
import Link from "next/link";

import { StatusBadge } from "@/components/shared/status-badge";
import { Badge } from "@/components/ui/badge";
import { formatQueueReason } from "@/lib/queue-reason";
import { cn } from "@/lib/utils";

type Props = {
  status: string;
  cancelReason?: string;
  supersededBy?: string;
  // Why a queued/running run isn't advancing (runs.queue_reason). Rendered as a
  // muted pill NEXT TO the status badge, not instead of it: unlike a supersede
  // (which is terminal and replaces the outcome), a queue reason explains a run
  // that is still live, so hiding its real status would lose information.
  queueReason?: string;
  className?: string;
  // When false, never wrap the superseded badge in a link — use inside a row that is
  // ALREADY a link (avoids nesting <a> in <a>). Default true.
  linkWinner?: boolean;
};

// RunStatusBadge renders the normal run-status badge, EXCEPT for a supersede-canceled
// run (#97): a newer revision won the lane, so a plain "Canceled" hides why. Show a
// muted "superseded by #N" in the canceled tone (reusing the outline variant),
// linking to the winning run when it's still around. Mirrors the muted "Canceling…"
// precedent in job-card.tsx.
export function RunStatusBadge({
  status,
  cancelReason,
  supersededBy,
  queueReason,
  className,
  linkWinner = true,
}: Props) {
  const superseded =
    status === "canceled" && (Boolean(supersededBy) || Boolean(cancelReason));
  if (!superseded) {
    // Only for a live run: a terminal run's queue_reason is a leftover from
    // when it was waiting, and rendering "Frozen: production" beside "Success"
    // would be actively misleading.
    const held =
      status === "queued" || status === "running"
        ? formatQueueReason(queueReason)
        : null;
    if (!held) {
      return <StatusBadge status={status} className={className} />;
    }
    return (
      <span className="inline-flex items-center gap-1.5">
        <StatusBadge status={status} className={className} />
        <Badge
          variant="outline"
          className="gap-1 border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-400"
          title={held.title}
        >
          {held.label}
        </Badge>
      </span>
    );
  }

  const label = cancelReason?.trim() || "superseded";
  const badge = (
    <Badge
      variant="outline"
      className={cn(
        "gap-1 border-muted-foreground/30 bg-muted/40 text-muted-foreground",
        className,
      )}
      title="This run was superseded by a newer revision in the same lane"
    >
      {label}
    </Badge>
  );

  if (!supersededBy || !linkWinner) {
    return badge;
  }
  return (
    <Link
      href={`/runs/${supersededBy}` as Route}
      className="rounded-sm hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
    >
      {badge}
    </Link>
  );
}
