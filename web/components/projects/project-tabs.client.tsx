"use client";

import Link from "next/link";
import type { Route } from "next";
import { usePathname } from "next/navigation";
import {
  GitBranch,
  History,
  KeyRound,
  Network,
  type LucideIcon,
} from "lucide-react";

import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";

type Tab = {
  label: string;
  href: (slug: string) => Route;
  match: (pathname: string, slug: string) => boolean;
  icon: LucideIcon;
};

// Tabs config is hard-coded because LucideIcon components don't
// serialise across the RSC boundary — same reason SettingsTabs
// keeps its list client-side. Order mirrors the user flow: see
// what runs, understand the graph, manage secrets, review runs.
const tabs: Tab[] = [
  {
    label: "Pipelines",
    href: (slug) => `/projects/${slug}` as Route,
    // Exact match: "/projects/<slug>" alone. A sub-route shouldn't
    // light up both Pipelines AND the sub-tab.
    match: (path, slug) => path === `/projects/${slug}`,
    icon: GitBranch,
  },
  {
    label: "VSM",
    href: (slug) => `/projects/${slug}/vsm` as Route,
    match: (path, slug) => path.startsWith(`/projects/${slug}/vsm`),
    icon: Network,
  },
  {
    label: "Secrets",
    href: (slug) => `/projects/${slug}/secrets` as Route,
    match: (path, slug) => path.startsWith(`/projects/${slug}/secrets`),
    icon: KeyRound,
  },
  {
    label: "Recent runs",
    href: (slug) => `/projects/${slug}/runs` as Route,
    match: (path, slug) => path.startsWith(`/projects/${slug}/runs`),
    icon: History,
  },
];

type Props = { slug: string };

// ProjectTabs is the top-level nav inside a project. Each
// trigger renders as a Next Link; the active one is decided
// by pathname matching so keyboard / URL / click stay in
// sync. "line" variant mimics GitHub's sub-navigation: no
// pill rail behind, just an underline on the active tab.
export function ProjectTabs({ slug }: Props) {
  const pathname = usePathname();
  const active = tabs.find((t) => t.match(pathname, slug))?.label ?? "Pipelines";

  return (
    <Tabs value={active}>
      <TabsList className="w-full justify-start bg-transparent p-0">
        {tabs.map((tab) => {
          const Icon = tab.icon;
          return (
            <TabsTrigger
              key={tab.label}
              value={tab.label}
              nativeButton={false}
              render={<Link href={tab.href(slug)} />}
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
