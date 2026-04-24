"use client";

import Link from "next/link";
import type { Route } from "next";
import { usePathname } from "next/navigation";

import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb";

// RouteBreadcrumbs turns the current URL into shadcn Breadcrumb
// components. Registry-driven: each segment is mapped to a human
// label (no "[slug]" leaking into the UI) and optionally to a link
// target. Segments not in the registry fall back to their raw
// value — safe for UUIDs under `/runs/<id>` where the raw id is
// what a power user would want to see anyway.
type LabelFn = (segment: string, segments: string[]) => string;
type BreadcrumbHref = (segments: string[]) => string | null;

type Entry = {
  label: LabelFn;
  // When href is null, the crumb is rendered as non-clickable
  // current-page text. Segments known to be prefixes (projects)
  // link to their own index; leaf routes (vsm, secrets) are text.
  href?: BreadcrumbHref;
};

const registry: Record<string, Entry> = {
  projects: {
    label: () => "Projects",
    href: () => "/projects",
  },
  runs: {
    label: () => "Runs",
    href: () => "/runs",
  },
  agents: {
    label: () => "Agents",
    href: () => "/agents",
  },
  vsm: {
    label: () => "VSM",
    href: () => null,
  },
  secrets: {
    label: () => "Secrets",
    href: () => null,
  },
  settings: {
    label: () => "Settings",
    href: () => "/settings",
  },
  retention: {
    label: () => "Retention",
    href: () => null,
  },
  webhooks: {
    label: () => "Webhooks",
    href: () => "/settings/webhooks",
  },
  health: {
    label: () => "Health",
    href: () => null,
  },
  integrations: {
    label: () => "Integrations",
    href: () => null,
  },
  github: {
    label: () => "GitHub",
    href: () => null,
  },
  auth: {
    label: () => "Auth",
    href: () => "/settings/auth",
  },
  account: {
    label: () => "Account",
    href: () => "/account",
  },
  admin: {
    label: () => "Admin",
    href: () => null,
  },
  users: {
    label: () => "Users",
    href: () => "/admin/users",
  },
  audit: {
    label: () => "Audit log",
    href: () => "/admin/audit",
  },
  plugins: {
    label: () => "Plugins",
    href: () => "/admin/plugins",
  },
};

export function RouteBreadcrumbs() {
  const pathname = usePathname();
  const segments = pathname.split("/").filter(Boolean);
  if (segments.length === 0) {
    return (
      <Breadcrumb>
        <BreadcrumbList>
          <BreadcrumbItem>
            <BreadcrumbPage>Dashboard</BreadcrumbPage>
          </BreadcrumbItem>
        </BreadcrumbList>
      </Breadcrumb>
    );
  }

  return (
    <Breadcrumb>
      <BreadcrumbList>
        <BreadcrumbItem className="hidden md:block">
          <BreadcrumbLink render={<Link href="/">Home</Link>} />
        </BreadcrumbItem>
        <BreadcrumbSeparator className="hidden md:block" />
        {segments.map((seg, i) => {
          const consumed = segments.slice(0, i + 1);
          const isLast = i === segments.length - 1;
          const meta = registry[seg];
          const label = meta?.label
            ? meta.label(seg, consumed)
            : decodeURIComponent(seg);
          const href = meta?.href
            ? meta.href(consumed)
            : "/" + consumed.join("/");

          return (
            <span key={consumed.join("/")} className="contents">
              <BreadcrumbItem>
                {isLast || !href ? (
                  <BreadcrumbPage className="truncate max-w-[220px]">
                    {label}
                  </BreadcrumbPage>
                ) : (
                  <BreadcrumbLink
                    render={
                      <Link href={href as Route} className="truncate max-w-[220px]">
                        {label}
                      </Link>
                    }
                  />
                )}
              </BreadcrumbItem>
              {!isLast ? <BreadcrumbSeparator /> : null}
            </span>
          );
        })}
      </BreadcrumbList>
    </Breadcrumb>
  );
}
