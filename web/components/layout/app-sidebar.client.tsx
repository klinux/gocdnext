"use client";

import type { ComponentType } from "react";
import Link from "next/link";
import type { Route } from "next";
import { usePathname } from "next/navigation";
import {
  Activity,
  Boxes,
  GitBranch,
  LayoutDashboard,
  Server,
  Settings,
} from "lucide-react";

import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
} from "@/components/ui/sidebar";

type NavItem = {
  label: string;
  href: string;
  icon: ComponentType<{ className?: string }>;
  // Soon items stay in the menu but route to "/" with a title so
  // users see the roadmap without dead links. Swap to false as each
  // page ships.
  comingSoon?: boolean;
};

// Keep this list in sync with the Fase 3 / UI roadmap. As UI.2/UI.3
// land, flip comingSoon flags and point href at the real route.
const primaryNav: NavItem[] = [
  { label: "Dashboard", href: "/", icon: LayoutDashboard },
  { label: "Projects", href: "/projects", icon: Boxes },
  { label: "Runs", href: "/runs", icon: Activity },
  { label: "Agents", href: "/agents", icon: Server },
];

const adminNav: NavItem[] = [
  { label: "Settings", href: "/", icon: Settings, comingSoon: true },
];

export function AppSidebar() {
  const pathname = usePathname();

  return (
    <Sidebar collapsible="icon">
      <SidebarHeader>
        <div className="flex items-center gap-2 px-2 py-1.5">
          <GitBranch className="size-5 shrink-0 text-primary" aria-hidden />
          <span className="truncate text-sm font-semibold tracking-tight">
            gocdnext
          </span>
        </div>
      </SidebarHeader>

      <SidebarContent>
        <SidebarGroup>
          <SidebarGroupLabel>Workspace</SidebarGroupLabel>
          <SidebarGroupContent>
            <SidebarMenu>
              {primaryNav.map((item) => (
                <SidebarNavItem
                  key={item.label}
                  item={item}
                  active={isActive(pathname, item)}
                />
              ))}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>

        <SidebarGroup>
          <SidebarGroupLabel>Admin</SidebarGroupLabel>
          <SidebarGroupContent>
            <SidebarMenu>
              {adminNav.map((item) => (
                <SidebarNavItem
                  key={item.label}
                  item={item}
                  active={isActive(pathname, item)}
                />
              ))}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>
      </SidebarContent>

      <SidebarFooter>
        <p className="px-2 py-1 text-[10px] uppercase tracking-wide text-muted-foreground">
          Control plane
        </p>
      </SidebarFooter>
    </Sidebar>
  );
}

function SidebarNavItem({ item, active }: { item: NavItem; active: boolean }) {
  const Icon = item.icon;
  return (
    <SidebarMenuItem>
      <SidebarMenuButton
        isActive={active}
        tooltip={item.comingSoon ? `${item.label} — coming soon` : item.label}
        render={
          <Link href={item.href as Route} aria-disabled={item.comingSoon}>
            <Icon className="size-4" />
            <span>{item.label}</span>
            {item.comingSoon ? (
              <span className="ml-auto text-[10px] uppercase tracking-wide text-muted-foreground">
                soon
              </span>
            ) : null}
          </Link>
        }
      />
    </SidebarMenuItem>
  );
}

// Active rule: `/` only matches home exactly; nested routes match
// the longest prefix. Safeguards against everything highlighting as
// active just because its href is `/`.
function isActive(pathname: string, item: NavItem): boolean {
  if (item.comingSoon) return false;
  if (item.href === "/") return pathname === "/";
  return pathname === item.href || pathname.startsWith(item.href + "/");
}
