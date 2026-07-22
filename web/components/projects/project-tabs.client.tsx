"use client";

import Link from "next/link";
import type { Route } from "next";
import { usePathname } from "next/navigation";
import {
  Bell,
  Clock,
  GitBranch,
  GitCompareArrows,
  HardDrive,
  History,
  KeyRound,
  Network,
  Rocket,
  Settings,
  ShieldAlert,
  type LucideIcon,
} from "lucide-react";

import { cn } from "@/lib/utils";

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
    label: "Caches",
    href: (slug) => `/projects/${slug}/caches` as Route,
    match: (path, slug) => path.startsWith(`/projects/${slug}/caches`),
    icon: HardDrive,
  },
  {
    label: "Notifications",
    href: (slug) => `/projects/${slug}/notifications` as Route,
    match: (path, slug) => path.startsWith(`/projects/${slug}/notifications`),
    icon: Bell,
  },
  {
    label: "Schedules",
    href: (slug) => `/projects/${slug}/crons` as Route,
    match: (path, slug) => path.startsWith(`/projects/${slug}/crons`),
    icon: Clock,
  },
  {
    label: "Recent runs",
    href: (slug) => `/projects/${slug}/runs` as Route,
    match: (path, slug) => path.startsWith(`/projects/${slug}/runs`),
    icon: History,
  },
  {
    label: "Security",
    href: (slug) => `/projects/${slug}/security` as Route,
    match: (path, slug) => path.startsWith(`/projects/${slug}/security`),
    icon: ShieldAlert,
  },
  {
    label: "Environments",
    href: (slug) => `/projects/${slug}/environments` as Route,
    match: (path, slug) => path.startsWith(`/projects/${slug}/environments`),
    icon: Rocket,
  },
  {
    label: "Rollouts",
    href: (slug) => `/projects/${slug}/rollouts` as Route,
    match: (path, slug) => path.startsWith(`/projects/${slug}/rollouts`),
    icon: GitCompareArrows,
  },
  {
    label: "Settings",
    href: (slug) => `/projects/${slug}/settings` as Route,
    match: (path, slug) => path.startsWith(`/projects/${slug}/settings`),
    icon: Settings,
  },
];

type Props = {
  slug: string;
  // showRollouts hides the Rollouts tab for a project that has no
  // rollout-aware deploy target — a service shipping a plain Deployment has
  // no Rollout to show, so the tab would only ever lead to a dead form. Also
  // false for a viewer (the rollouts read is maintainer-gated, so they could
  // not load it anyway). The route stays reachable by URL.
  showRollouts?: boolean;
};

// ProjectTabs is the top-level nav inside a project — a row of Next
// Links (real navigation, so `nav` + aria-current, not a tablist). The
// active section is decided by pathname matching. Styled as a segmented
// row: transparent track, the active tab takes the brand teal tint —
// the shared pill language (see lib/tab-pill for the shadcn Tabs variants).
export function ProjectTabs({ slug, showRollouts = false }: Props) {
  const pathname = usePathname();
  const visible = tabs.filter((t) => t.label !== "Rollouts" || showRollouts);

  return (
    <nav aria-label="Project sections" className="flex flex-wrap gap-1">
      {visible.map((tab) => {
        const Icon = tab.icon;
        const isActive = tab.match(pathname, slug);
        return (
          <Link
            key={tab.label}
            href={tab.href(slug)}
            aria-current={isActive ? "page" : undefined}
            className={cn(
              "inline-flex items-center gap-1.5 rounded-md border border-transparent px-3 py-1.5 text-sm font-medium transition-colors",
              isActive
                ? "border-primary/30 bg-primary/10 text-primary"
                : "text-muted-foreground hover:bg-accent/50 hover:text-foreground",
            )}
          >
            <Icon className="size-3.5 opacity-80" aria-hidden />
            {tab.label}
          </Link>
        );
      })}
    </nav>
  );
}
