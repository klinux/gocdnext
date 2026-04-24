"use client";

import { useEffect, useRef, useState } from "react";
import type { DateRange } from "react-day-picker";
import { CalendarIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Calendar } from "@/components/ui/calendar";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import { cn } from "@/lib/utils";

type Props = {
  // Initial "from" and "to" values from the URL query. Both in
  // ISO date form (YYYY-MM-DD). Empty string = open-ended on
  // that side.
  from: string;
  to: string;
};

type Preset = { label: string; days: number };

const presets: Preset[] = [
  { label: "Today", days: 0 },
  { label: "Last 7 days", days: 7 },
  { label: "Last 30 days", days: 30 },
];

// AuditDateRange is a single Button trigger that opens a popover
// carrying a 2-month Calendar in range mode plus a left-side
// preset list. The selected range is written into hidden inputs
// so the parent GET form submits whatever the user picked —
// keeping filters bookmarkable via URL.
//
// A form-submit button lives inside the popover footer: range
// mutations don't auto-submit because typical flow is "pick
// from, pick to, then apply". Presets do auto-submit since
// they're a full commitment.
export function AuditDateRange({ from, to }: Props) {
  const initialRange: DateRange | undefined = (() => {
    if (!from && !to) return undefined;
    return {
      from: from ? parseISODate(from) : undefined,
      to: to ? parseISODate(to) : undefined,
    };
  })();

  const [open, setOpen] = useState(false);
  const [range, setRange] = useState<DateRange | undefined>(initialRange);
  const fromRef = useRef<HTMLInputElement>(null);
  const toRef = useRef<HTMLInputElement>(null);

  // Keep hidden inputs in sync with the picker state so the
  // enclosing GET form has the right values at submit time.
  useEffect(() => {
    if (fromRef.current) {
      fromRef.current.value = range?.from ? toISODate(range.from) : "";
    }
    if (toRef.current) {
      toRef.current.value = range?.to
        ? toISODate(addDays(range.to, 1))
        : range?.from
          ? toISODate(addDays(range.from, 1))
          : "";
    }
  }, [range]);

  const applyPreset = (days: number) => {
    const now = new Date();
    const end = startOfDay(now);
    const start = addDays(end, -days);
    setRange({ from: start, to: end });
    setOpen(false);
    // Give the hidden inputs a tick to absorb the new values
    // before we requestSubmit.
    setTimeout(() => fromRef.current?.form?.requestSubmit(), 0);
  };

  const clear = () => {
    setRange(undefined);
    setOpen(false);
    setTimeout(() => fromRef.current?.form?.requestSubmit(), 0);
  };

  const apply = () => {
    setOpen(false);
    setTimeout(() => fromRef.current?.form?.requestSubmit(), 0);
  };

  const label = formatRangeLabel(range);

  return (
    <div className="space-y-1">
      <label className="text-[11px] font-medium text-muted-foreground">
        Date range
      </label>
      {/* Hidden inputs ride inside the enclosing <form> so GET
          picks them up. Names match the server-side params. */}
      <input type="hidden" name="from" ref={fromRef} defaultValue={from} />
      <input type="hidden" name="to" ref={toRef} defaultValue={to} />

      <Popover open={open} onOpenChange={setOpen}>
        <PopoverTrigger
          render={
            <Button
              variant="outline"
              size="sm"
              className={cn(
                "h-8 w-full justify-start text-xs font-normal",
                !range && "text-muted-foreground",
              )}
            >
              <CalendarIcon className="mr-2 size-3.5 shrink-0" aria-hidden />
              <span className="truncate">{label}</span>
            </Button>
          }
        />
        <PopoverContent className="flex p-0" align="start">
          <div className="flex flex-col border-r bg-muted/20 p-2">
            {presets.map((p) => (
              <button
                key={p.label}
                type="button"
                onClick={() => applyPreset(p.days)}
                className="rounded px-3 py-1.5 text-left text-xs hover:bg-accent hover:text-accent-foreground"
              >
                {p.label}
              </button>
            ))}
            <div className="my-1 border-t" />
            <button
              type="button"
              onClick={clear}
              className="rounded px-3 py-1.5 text-left text-xs text-muted-foreground hover:bg-accent hover:text-accent-foreground"
            >
              Clear
            </button>
          </div>
          <div>
            <Calendar
              mode="range"
              numberOfMonths={2}
              selected={range}
              onSelect={setRange}
              defaultMonth={range?.from}
            />
            <div className="flex items-center justify-end gap-2 border-t p-2">
              <Button
                variant="ghost"
                size="sm"
                onClick={() => setOpen(false)}
                className="h-7 text-xs"
              >
                Cancel
              </Button>
              <Button
                size="sm"
                onClick={apply}
                disabled={!range?.from}
                className="h-7 text-xs"
              >
                Apply
              </Button>
            </div>
          </div>
        </PopoverContent>
      </Popover>
    </div>
  );
}

function parseISODate(s: string): Date | undefined {
  const m = s.match(/^(\d{4})-(\d{2})-(\d{2})$/);
  if (!m) return undefined;
  const [, y, mo, d] = m;
  return new Date(Number(y), Number(mo) - 1, Number(d));
}

function toISODate(d: Date): string {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${y}-${m}-${day}`;
}

function startOfDay(d: Date): Date {
  const out = new Date(d);
  out.setHours(0, 0, 0, 0);
  return out;
}

function addDays(d: Date, n: number): Date {
  const out = new Date(d);
  out.setDate(out.getDate() + n);
  return out;
}

function formatRangeLabel(r: DateRange | undefined): string {
  if (!r?.from) return "All time";
  const fmt = (d: Date) =>
    d.toLocaleDateString(undefined, {
      month: "short",
      day: "numeric",
      year: "numeric",
    });
  if (!r.to || isSameDay(r.from, r.to)) {
    return fmt(r.from);
  }
  return `${fmt(r.from)} — ${fmt(r.to)}`;
}

function isSameDay(a: Date, b: Date): boolean {
  return (
    a.getFullYear() === b.getFullYear() &&
    a.getMonth() === b.getMonth() &&
    a.getDate() === b.getDate()
  );
}
