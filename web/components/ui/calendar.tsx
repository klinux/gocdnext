"use client";

import { ChevronLeft, ChevronRight } from "lucide-react";
import { DayPicker } from "react-day-picker";

import { cn } from "@/lib/utils";

// Calendar wraps react-day-picker with shadcn-ish classes tuned
// for our token palette. Passes through all DayPicker props so
// callers can flip between single, range, multiple modes + hook
// up onSelect handlers.
export type CalendarProps = React.ComponentProps<typeof DayPicker>;

export function Calendar({
  className,
  classNames,
  showOutsideDays = true,
  ...props
}: CalendarProps) {
  return (
    <DayPicker
      showOutsideDays={showOutsideDays}
      className={cn("p-3", className)}
      classNames={{
        months: "flex flex-col sm:flex-row gap-4",
        month: "space-y-3",
        month_caption: "flex items-center justify-center text-sm font-medium",
        caption_label: "text-sm font-medium",
        nav: "flex items-center gap-1",
        button_previous: cn(
          "inline-flex size-7 items-center justify-center rounded-md border bg-transparent text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground disabled:opacity-40",
        ),
        button_next: cn(
          "inline-flex size-7 items-center justify-center rounded-md border bg-transparent text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground disabled:opacity-40",
        ),
        month_grid: "w-full border-collapse",
        weekdays: "flex",
        weekday:
          "text-muted-foreground rounded-md w-8 font-normal text-[0.8rem]",
        week: "flex w-full mt-1",
        day: "relative h-8 w-8 text-center text-sm",
        day_button:
          "inline-flex size-8 items-center justify-center rounded-md font-normal transition-colors hover:bg-accent hover:text-accent-foreground aria-selected:opacity-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
        selected:
          "bg-primary text-primary-foreground hover:bg-primary hover:text-primary-foreground focus:bg-primary focus:text-primary-foreground",
        today: "bg-accent text-accent-foreground",
        outside: "text-muted-foreground/60 aria-selected:text-muted-foreground",
        disabled: "text-muted-foreground opacity-50",
        range_start:
          "rounded-l-md bg-primary text-primary-foreground",
        range_middle:
          "aria-selected:bg-accent aria-selected:text-accent-foreground",
        range_end:
          "rounded-r-md bg-primary text-primary-foreground",
        hidden: "invisible",
        ...classNames,
      }}
      components={{
        Chevron: ({ orientation }) =>
          orientation === "left" ? (
            <ChevronLeft className="size-4" aria-hidden />
          ) : (
            <ChevronRight className="size-4" aria-hidden />
          ),
      }}
      {...props}
    />
  );
}
