"use client";

import Link from "next/link";
import type { Route } from "next";
import { usePathname } from "next/navigation";

import { cn } from "@/lib/utils";

type Tab = { label: string; href: Route; matchPrefix: string };

export function SettingsTabs({ tabs }: { tabs: Tab[] }) {
  const pathname = usePathname();
  return (
    <nav className="flex items-center gap-1 border-b">
      {tabs.map((tab) => {
        const active = pathname.startsWith(tab.matchPrefix);
        return (
          <Link
            key={tab.label}
            href={tab.href}
            className={cn(
              "relative -mb-px inline-flex h-9 items-center rounded-none border-b-2 px-3 text-sm font-medium transition-colors",
              active
                ? "border-foreground text-foreground"
                : "border-transparent text-muted-foreground hover:text-foreground",
            )}
          >
            {tab.label}
          </Link>
        );
      })}
    </nav>
  );
}
