"use client";

import { useState } from "react";
import { ChevronDown, Tag } from "lucide-react";

import { FilterPill } from "@/components/projects/project-filter-pills";
import { labelText } from "@/components/projects/project-ui-helpers";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";

export type LabelChip = {
  id: string;
  key: string;
  value: string;
  count: number;
};

type Props = {
  labels: LabelChip[];
  // Selected label id (its stable tuple identity), or null when no
  // label filter is active.
  selected: string | null;
  onSelect: (id: string | null) => void;
  // How many chips stay inline before the rest collapse into the
  // searchable "+N" menu. Keeps the toolbar from becoming a wall of
  // chips as the label vocabulary (team:*, env:*, …) grows.
  maxInline?: number;
};

const DEFAULT_MAX_INLINE = 4;

// LabelFilterPills renders the project label filters as inline chips up
// to `maxInline`, collapsing the overflow into a "+N" popover with a
// search box. The active filter is ALWAYS shown inline (even when it
// would otherwise live in the overflow) so the current selection never
// hides behind the menu.
export function LabelFilterPills({
  labels,
  selected,
  onSelect,
  maxInline = DEFAULT_MAX_INLINE,
}: Props) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");

  if (labels.length === 0) return null;

  // Fits without a menu — render everything inline (the common case).
  if (labels.length <= maxInline) {
    return (
      <>
        {labels.map((c) => (
          <LabelChipPill
            key={c.id}
            chip={c}
            selected={selected}
            onSelect={onSelect}
          />
        ))}
      </>
    );
  }

  // Pull the active label into the inline set if it would otherwise be
  // hidden in the overflow, so the current selection stays visible.
  let inline = labels.slice(0, maxInline);
  const active = selected ? labels.find((l) => l.id === selected) : undefined;
  if (active && !inline.some((l) => l.id === active.id)) {
    inline = [active, ...labels.slice(0, maxInline - 1)];
  }
  const inlineIds = new Set(inline.map((l) => l.id));
  const overflowCount = labels.length - inlineIds.size;

  // Search spans the FULL set (not just the overflow) so the menu can
  // find any label, including the ones already shown inline.
  const q = query.trim().toLowerCase();
  const results = q
    ? labels.filter((l) => labelText(l.key, l.value).toLowerCase().includes(q))
    : labels;

  const close = () => {
    setOpen(false);
    setQuery("");
  };

  return (
    <>
      {inline.map((c) => (
        <LabelChipPill
          key={c.id}
          chip={c}
          selected={selected}
          onSelect={onSelect}
        />
      ))}
      <Popover
        open={open}
        onOpenChange={(next) => {
          setOpen(next);
          if (!next) setQuery("");
        }}
      >
        <PopoverTrigger
          render={
            <button
              type="button"
              aria-label={`Show ${overflowCount} more label filters`}
              className={cn(
                "inline-flex items-center gap-1 rounded-full border px-2.5 py-1 text-xs font-medium transition-colors",
                "border-border bg-background text-muted-foreground hover:border-foreground/30 hover:text-foreground",
              )}
            >
              <span>+{overflowCount}</span>
              <ChevronDown className="size-3" aria-hidden />
            </button>
          }
        />
        <PopoverContent align="start" className="w-64 p-0">
          <div className="p-2">
            <Input
              // autoFocus so the user can type immediately on open.
              autoFocus
              value={query}
              // base-ui Input is controlled via onValueChange, not the
              // native onChange (see components/ui/input.tsx).
              onValueChange={(value) => setQuery(value)}
              placeholder="Search labels…"
              aria-label="Search labels"
              className="h-8"
            />
          </div>
          <div role="listbox" className="max-h-64 overflow-y-auto p-1">
            {results.length === 0 ? (
              <p className="px-2 py-3 text-center text-xs text-muted-foreground">
                No labels match.
              </p>
            ) : (
              results.map((c) => {
                const isSelected = selected === c.id;
                return (
                  <button
                    key={c.id}
                    type="button"
                    role="option"
                    aria-selected={isSelected}
                    onClick={() => {
                      onSelect(isSelected ? null : c.id);
                      close();
                    }}
                    className={cn(
                      "flex w-full items-center justify-between gap-2 rounded-sm px-2 py-1.5 text-left text-xs hover:bg-muted",
                      isSelected && "bg-muted font-medium text-foreground",
                    )}
                  >
                    <span className="flex min-w-0 items-center gap-1.5">
                      <Tag className="size-3 shrink-0" aria-hidden />
                      <span className="truncate">
                        {labelText(c.key, c.value)}
                      </span>
                    </span>
                    <span className="shrink-0 rounded-full bg-muted px-1.5 text-[10px] tabular-nums">
                      {c.count}
                    </span>
                  </button>
                );
              })
            )}
          </div>
        </PopoverContent>
      </Popover>
    </>
  );
}

function LabelChipPill({
  chip,
  selected,
  onSelect,
}: {
  chip: LabelChip;
  selected: string | null;
  onSelect: (id: string | null) => void;
}) {
  return (
    <FilterPill
      label={labelText(chip.key, chip.value)}
      count={chip.count}
      active={selected === chip.id}
      onClick={() => onSelect(selected === chip.id ? null : chip.id)}
      tone="neutral"
      icon={<Tag className="size-3" />}
    />
  );
}
