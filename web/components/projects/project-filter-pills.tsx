"use client";

import { cn } from "@/lib/utils";
import type { StatusTone } from "@/lib/status";
import { activePillClasses } from "@/components/projects/project-ui-helpers";

type Props = {
  label: string;
  count: number;
  active: boolean;
  onClick: () => void;
  tone: "all" | StatusTone;
  icon?: React.ReactNode;
};

// FilterPill is the status/provider chip at the top of the projects
// page. Active variant uses the status tone on the border + a
// slightly intensified fill; idle is neutral border + muted text.
export function FilterPill({
  label,
  count,
  active,
  onClick,
  tone,
  icon,
}: Props) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      className={cn(
        "inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-medium transition-colors",
        active
          ? activePillClasses[tone]
          : "border-border bg-background text-muted-foreground hover:border-foreground/30 hover:text-foreground",
      )}
    >
      {icon}
      <span>{label}</span>
      <span
        className={cn(
          "rounded-full px-1.5 text-[10px] tabular-nums",
          active ? "bg-background/40" : "bg-muted",
        )}
      >
        {count}
      </span>
    </button>
  );
}
