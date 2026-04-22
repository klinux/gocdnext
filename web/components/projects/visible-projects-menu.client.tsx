"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { Eye, EyeOff } from "lucide-react";
import { toast } from "sonner";

import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { savePreferences } from "@/server/actions/account";
import type { ProjectSummary, UserPreferences } from "@/types/api";

type Props = {
  projects: ProjectSummary[];
  initialHidden: string[];
  // Called every time the hidden set changes locally so the parent
  // can apply the filter immediately, before the debounced save
  // settles — local UX beats a 500ms save round-trip.
  onLocalChange: (hidden: string[]) => void;
};

const SAVE_DEBOUNCE_MS = 500;

// VisibleProjectsMenu is the dropdown the user uses to pick which
// projects they want on the list. Semantics are a hide-list
// (unchecked = hidden), so freshly-applied projects default to
// visible for every user. Changes flush locally first, then save
// to the server on a 500ms debounce — prevents hammering the API
// while the user toggles through several items.
export function VisibleProjectsMenu({
  projects,
  initialHidden,
  onLocalChange,
}: Props) {
  const [hidden, setHidden] = useState<Set<string>>(
    () => new Set(initialHidden),
  );
  const [saving, setSaving] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const latestPayload = useRef<UserPreferences>({
    hidden_projects: initialHidden,
  });

  const sorted = useMemo(
    () =>
      [...projects].sort((a, b) =>
        a.name.localeCompare(b.name, undefined, { sensitivity: "base" }),
      ),
    [projects],
  );

  const hiddenCount = hidden.size;
  const visibleCount = projects.length - hiddenCount;

  // Schedule a debounced save. The ref trick means rapid clicks
  // all coalesce into a single server write after the user pauses
  // for SAVE_DEBOUNCE_MS — the last captured state wins.
  const scheduleSave = (next: Set<string>) => {
    latestPayload.current = { hidden_projects: [...next] };
    if (timer.current) clearTimeout(timer.current);
    timer.current = setTimeout(async () => {
      setSaving(true);
      const res = await savePreferences(latestPayload.current);
      setSaving(false);
      if (!res.ok) {
        toast.error(`Couldn't save preferences: ${res.error}`);
      }
    }, SAVE_DEBOUNCE_MS);
  };

  useEffect(() => {
    return () => {
      if (timer.current) clearTimeout(timer.current);
    };
  }, []);

  // Side-effects (parent setState + debounced save) must run
  // OUTSIDE React's state-updater callback — calling
  // `onLocalChange` inside setHidden's updater triggers a setState
  // on the parent during this component's render phase, which
  // React flags as "Cannot update a component while rendering".
  // Computing `next` from the current render's `hidden` is safe
  // here because clicks don't batch state updates across frames
  // — each toggle is its own event tick.
  const commit = (next: Set<string>) => {
    setHidden(next);
    onLocalChange([...next]);
    scheduleSave(next);
  };

  const toggle = (projectID: string) => {
    const next = new Set(hidden);
    if (next.has(projectID)) next.delete(projectID);
    else next.add(projectID);
    commit(next);
  };

  const showAll = () => {
    commit(new Set<string>());
  };

  const hideAll = () => {
    commit(new Set(projects.map((p) => p.id)));
  };

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button
            variant="outline"
            size="sm"
            className={cn("gap-1.5", saving && "opacity-70")}
            title="Choose which projects appear on this list"
          >
            {hiddenCount > 0 ? (
              <EyeOff className="size-3.5" aria-hidden />
            ) : (
              <Eye className="size-3.5" aria-hidden />
            )}
            <span>
              Visible {visibleCount} / {projects.length}
            </span>
          </Button>
        }
      />
      <DropdownMenuContent align="end" className="max-h-[70vh] w-72 overflow-y-auto">
        {/* Plain header div instead of DropdownMenuLabel — base-ui's
            Menu.GroupLabel requires a Menu.Group ancestor, and we
            don't need the a11y-group semantics for a simple
            section-title string at the top of the menu. */}
        <div className="flex items-center justify-between gap-2 px-2 py-1.5 text-xs font-medium text-foreground">
          <span>Show on this page</span>
          <span className="font-mono text-[10px] text-muted-foreground">
            {saving ? "saving…" : ""}
          </span>
        </div>
        <div className="flex items-center gap-1 px-2 pb-1 text-[11px]">
          <button
            type="button"
            onClick={showAll}
            className="rounded px-1.5 py-0.5 text-muted-foreground hover:bg-accent hover:text-foreground"
          >
            show all
          </button>
          <span className="text-muted-foreground/40">·</span>
          <button
            type="button"
            onClick={hideAll}
            className="rounded px-1.5 py-0.5 text-muted-foreground hover:bg-accent hover:text-foreground"
          >
            hide all
          </button>
        </div>
        <DropdownMenuSeparator />
        {sorted.map((p) => {
          const isVisible = !hidden.has(p.id);
          return (
            <DropdownMenuCheckboxItem
              key={p.id}
              checked={isVisible}
              onCheckedChange={() => toggle(p.id)}
              // Stop propagation so the menu stays open while the
              // user toggles multiple projects in one pass.
              onSelect={(e) => e.preventDefault()}
              className="text-xs"
            >
              <span className="truncate font-mono">{p.name}</span>
            </DropdownMenuCheckboxItem>
          );
        })}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
