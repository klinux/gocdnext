"use client";

import Link from "next/link";
import type { Route } from "next";
import { usePathname } from "next/navigation";
import {
  Activity,
  Archive,
  KeyRound,
  Lock,
  Plug,
  Webhook,
  type LucideIcon,
} from "lucide-react";

import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";

type Tab = {
  label: string;
  href: Route;
  matchPrefix: string;
  icon: LucideIcon;
};

// Tabs config lives on the client because LucideIcon components
// can't be serialised across the RSC boundary — passing them as
// props from the server layout fails with "only plain objects".
// Keeping the list here means the server layout just renders the
// component with no props, and updates go in one file.
const tabs: Tab[] = [
  { label: "Health", href: "/settings/health" as Route, matchPrefix: "/settings/health", icon: Activity },
  { label: "Webhooks", href: "/settings/webhooks" as Route, matchPrefix: "/settings/webhooks", icon: Webhook },
  { label: "Retention", href: "/settings/retention" as Route, matchPrefix: "/settings/retention", icon: Archive },
  { label: "Integrations", href: "/settings/integrations" as Route, matchPrefix: "/settings/integrations", icon: Plug },
  { label: "Auth", href: "/settings/auth" as Route, matchPrefix: "/settings/auth", icon: Lock },
  { label: "Secrets", href: "/settings/secrets" as Route, matchPrefix: "/settings/secrets", icon: KeyRound },
];

// SettingsTabs drives base-ui's controlled Tabs with the URL —
// each trigger renders as a <Link>, so clicking navigates while
// the active state flips off pathname matching. Rendered vertical
// with icons: /settings has a handful of sections that don't read
// well as a long horizontal strip, and icons give the sidebar a
// scan-first shape.
export function SettingsTabs() {
  const pathname = usePathname();
  const active =
    tabs.find((t) => pathname.startsWith(t.matchPrefix))?.matchPrefix ??
    tabs[0]?.matchPrefix ??
    "";

  return (
    <Tabs value={active} orientation="vertical" className="md:w-48 md:shrink-0">
      {/*
        Drop the muted rail behind the tabs — we want each item to
        feel like a link until it's active, and then only the
        active one carries a pill. `bg-transparent p-0` strips the
        TabsList default; triggers get a bg-accent override on
        data-active so the pill stays legible against a bare
        background.
      */}
      <TabsList className="w-full bg-transparent p-0">
        {tabs.map((tab) => {
          const Icon = tab.icon;
          return (
            <TabsTrigger
              key={tab.label}
              value={tab.matchPrefix}
              nativeButton={false}
              render={<Link href={tab.href} />}
              className="data-active:bg-accent data-active:text-accent-foreground dark:data-active:bg-accent"
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
