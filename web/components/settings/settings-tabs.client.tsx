"use client";

import Link from "next/link";
import type { Route } from "next";
import { usePathname } from "next/navigation";
import type { LucideIcon } from "lucide-react";

import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";

export type SettingsTab = {
  label: string;
  href: Route;
  matchPrefix: string;
  icon: LucideIcon;
};

// SettingsTabs drives base-ui's controlled Tabs with the URL —
// each trigger renders as a <Link>, so clicking navigates while
// the active state flips off pathname matching. Rendered vertical
// with icons: /settings has a handful of sections that don't read
// well as a long horizontal strip, and icons give the sidebar a
// scan-first shape.
export function SettingsTabs({ tabs }: { tabs: SettingsTab[] }) {
  const pathname = usePathname();
  const active =
    tabs.find((t) => pathname.startsWith(t.matchPrefix))?.matchPrefix ??
    tabs[0]?.matchPrefix ??
    "";

  return (
    <Tabs value={active} orientation="vertical" className="md:w-48 md:shrink-0">
      <TabsList className="w-full">
        {tabs.map((tab) => {
          const Icon = tab.icon;
          return (
            <TabsTrigger
              key={tab.label}
              value={tab.matchPrefix}
              // nativeButton=false tells base-ui we're swapping in a
              // non-<button> element (a Next Link) — it skips the
              // native-button accessibility assertion while keeping
              // keyboard activation wired via the Link.
              nativeButton={false}
              render={<Link href={tab.href} />}
            >
              <Icon className="size-4" aria-hidden />
              {tab.label}
            </TabsTrigger>
          );
        })}
      </TabsList>
    </Tabs>
  );
}
