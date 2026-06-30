"use client";

import { useTransition } from "react";
import { useRouter } from "next/navigation";
import { ChevronDown } from "lucide-react";
import { toast } from "sonner";

import { cn } from "@/lib/utils";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { setFindingState } from "@/server/actions/security";

type FindingState = "open" | "dismissed" | "false_positive" | "accepted";

const STATES: FindingState[] = ["open", "dismissed", "false_positive", "accepted"];

const LABEL: Record<FindingState, string> = {
  open: "Open",
  dismissed: "Dismissed",
  false_positive: "False positive",
  accepted: "Accepted risk",
};

// Trigger tint: open is neutral; accepted stays visibly amber (acknowledged
// risk, not silenced); dismissed/false_positive read as muted/resolved.
const TONE: Record<FindingState, string> = {
  open: "bg-muted text-muted-foreground",
  dismissed: "bg-muted text-muted-foreground",
  false_positive: "bg-muted text-muted-foreground",
  accepted: "bg-amber-500/15 text-amber-600 dark:text-amber-400",
};

type Props = { slug: string; stateId: number; state: string };

// FindingStateMenu is the per-finding triage control: a small dropdown to set
// open / dismiss / false-positive / accept. RBAC (maintainer+) is enforced
// server-side; a 403 is toasted. State persists by identity across scans.
export function FindingStateMenu({ slug, stateId, state }: Props) {
  const router = useRouter();
  const [pending, startTransition] = useTransition();
  const current = (STATES.includes(state as FindingState) ? state : "open") as FindingState;

  const apply = (next: FindingState) => {
    if (next === current) return;
    // A reason is optional but useful for dismiss/FP/accept (it's audited).
    const reason = next === "open" ? "" : window.prompt(`Reason for "${LABEL[next]}" (optional):`) ?? "";
    startTransition(async () => {
      const res = await setFindingState({ slug, stateId, state: next, reason });
      if (!res.ok) {
        toast.error(
          res.error.includes("403")
            ? "You need the maintainer role to change a finding's state"
            : res.error,
        );
        return;
      }
      toast.success(`Marked ${LABEL[next].toLowerCase()}`);
      router.refresh();
    });
  };

  // No identity row (shouldn't happen post-ingest) → render a static badge.
  if (stateId === 0) {
    return (
      <span className="inline-flex rounded-full bg-muted px-2 py-0.5 text-[11px] text-muted-foreground">
        {LABEL[current]}
      </span>
    );
  }

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        disabled={pending}
        aria-label="Change finding state"
        className={cn(
          "inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[11px] font-medium outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50",
          TONE[current],
        )}
      >
        {LABEL[current]}
        <ChevronDown className="size-3 opacity-70" aria-hidden />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="min-w-[160px]">
        {STATES.map((s) => (
          <DropdownMenuItem key={s} onClick={() => apply(s)} disabled={pending || s === current}>
            {LABEL[s]}
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
