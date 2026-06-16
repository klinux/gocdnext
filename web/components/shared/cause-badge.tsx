import {
  ArrowRight,
  Clock,
  GitCommit,
  GitPullRequest,
  type LucideIcon,
  PlayCircle,
  Tag,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

type Props = {
  // The run's trigger cause, as stored on runs.cause: push /
  // pull_request / tag / manual / cron|schedule / upstream.
  cause: string;
  className?: string;
};

// causeMeta maps a trigger cause to a short label + icon. Unknown
// causes fall back to the raw string with no icon, so a new server
// cause renders sanely (as text) instead of blank until the UI
// catches up.
function causeMeta(cause: string): { label: string; Icon: LucideIcon | null } {
  switch (cause) {
    case "push":
      return { label: "Push", Icon: GitCommit };
    case "pull_request":
      return { label: "PR", Icon: GitPullRequest };
    case "tag":
      return { label: "Tag", Icon: Tag };
    case "manual":
      return { label: "Manual", Icon: PlayCircle };
    case "cron":
    case "schedule":
      return { label: "Schedule", Icon: Clock };
    case "upstream":
      return { label: "Upstream", Icon: ArrowRight };
    default:
      return { label: cause, Icon: null };
  }
}

// CauseBadge shows HOW a run was triggered, with a distinct icon per
// cause — so a pull_request run reads differently from a push/tag at a
// glance (the bare `cause` string was ambiguous). Sibling of
// StatusBadge; subtle outline variant so it sits beside the status pill
// without competing with it.
export function CauseBadge({ cause, className }: Props) {
  const { label, Icon } = causeMeta(cause);
  return (
    <Badge
      variant="outline"
      className={cn("inline-flex items-center gap-1 font-normal", className)}
    >
      {Icon ? <Icon className="h-3 w-3" aria-hidden /> : null}
      {label}
    </Badge>
  );
}
