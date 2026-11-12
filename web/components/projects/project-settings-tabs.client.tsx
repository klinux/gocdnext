"use client";

import type { ReactNode } from "react";
import {
  Package,
  Settings2,
  ShieldCheck,
  SquareCheckBig,
  type LucideIcon,
} from "lucide-react";

import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

// Icon components can't cross the RSC boundary as props ("only plain objects"),
// so the server passes an icon KEY and we map it here — same pattern as the
// top-level SettingsTabs.
const ICONS: Record<string, LucideIcon> = {
  general: Settings2,
  checks: SquareCheckBig,
  build: Package,
  compliance: ShieldCheck,
};

export type ProjectSettingsTab = {
  value: string;
  label: string;
  /** Icon key resolved client-side (see ICONS). */
  icon?: string;
  /** Server-rendered section cards for this tab. */
  content: ReactNode;
};

// Vertical, in-page tabs for the project settings sections. The page had grown
// into a long scroll of cards; grouping them behind a left rail keeps each area
// scannable without changing any section's own behavior. The section cards are
// rendered on the server and handed in as `content` slots, so this component
// stays purely presentational (no data fetching, no section logic).
export function ProjectSettingsTabs({ tabs }: { tabs: ProjectSettingsTab[] }) {
  if (tabs.length === 0) return null;
  return (
    <Tabs
      defaultValue={tabs[0]?.value}
      orientation="vertical"
      // Stack on mobile (rail on top, panel below); side-by-side from md up. A
      // vertical Tabs root stays flex-row by default, which would squeeze the
      // panel on a narrow viewport.
      className="flex-col items-stretch gap-4 md:flex-row md:items-start md:gap-6"
    >
      {/*
        Bare left rail (no muted track): each item reads as a link until it's
        active, when it carries a primary pill — same language as the top-level
        settings nav.
      */}
      <TabsList className="w-full bg-transparent p-0 md:w-56 md:shrink-0">
        {tabs.map((tab) => {
          const Icon = tab.icon ? ICONS[tab.icon] : undefined;
          return (
            <TabsTrigger
              key={tab.value}
              value={tab.value}
              className="data-active:border-primary/30 data-active:bg-primary/10 data-active:text-primary dark:data-active:bg-primary/10"
            >
              {Icon ? <Icon className="size-4" aria-hidden /> : null}
              {tab.label}
            </TabsTrigger>
          );
        })}
      </TabsList>
      <div className="w-full min-w-0 flex-1">
        {tabs.map((tab) => (
          // keepMounted: inactive panels stay mounted (hidden) so a section's
          // local React state — an unsaved edit in Labels, Polling, Compliance,
          // etc. — survives switching tabs instead of resetting on remount.
          <TabsContent
            key={tab.value}
            value={tab.value}
            keepMounted
            className="space-y-6"
          >
            {tab.content}
          </TabsContent>
        ))}
      </div>
    </Tabs>
  );
}
