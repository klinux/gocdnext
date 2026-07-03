import type { Route } from "next";
import Link from "next/link";

import { StatusBadge } from "@/components/shared/status-badge";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

type Props = {
  status: string;
  cancelReason?: string;
  supersededBy?: string;
  className?: string;
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
  className,
}: Props) {
  const superseded =
    status === "canceled" && (Boolean(supersededBy) || Boolean(cancelReason));
  if (!superseded) {
    return <StatusBadge status={status} className={className} />;
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

  if (!supersededBy) {
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
