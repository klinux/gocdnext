"use client";

import Link from "next/link";
import type { Route } from "next";
import { usePathname } from "next/navigation";

import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";

type Tab = { label: string; href: Route; matchPrefix: string };

// SettingsTabs drives base-ui's controlled Tabs with the URL —
// each trigger renders as a <Link>, so clicking navigates while
// the active state flips off pathname matching. The "line"
// variant matches the previous custom look (no coloured pill
// behind the active tab, just the underline).
export function SettingsTabs({ tabs }: { tabs: Tab[] }) {
  const pathname = usePathname();
  const active =
    tabs.find((t) => pathname.startsWith(t.matchPrefix))?.matchPrefix ??
    tabs[0]?.matchPrefix ??
    "";

  return (
    <Tabs value={active}>
      <TabsList>
        {tabs.map((tab) => (
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
            {tab.label}
          </TabsTrigger>
        ))}
      </TabsList>
    </Tabs>
  );
}
