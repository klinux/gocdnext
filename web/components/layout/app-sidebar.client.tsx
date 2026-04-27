"use client";

import type { ComponentType } from "react";
import Link from "next/link";
import type { Route } from "next";
import { usePathname } from "next/navigation";
import {
  Activity,
  BookOpen,
  Boxes,
  ClipboardList,
  Cpu,
  KeyRound,
  LayoutDashboard,
  Package,
  Server,
  Settings,
  Users,
  UsersRound,
} from "lucide-react";

import { Logo, Wordmark } from "@/components/brand/logo";
import { SidebarUserMenu } from "@/components/layout/sidebar-user-menu.client";
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
import type { CurrentUser } from "@/types/api";

type Props = {
  user?: CurrentUser;
  loginBase?: string;
};

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
  // Plugin catalog is documentation-first: every dev authoring a
  // pipeline needs to know which `uses:` is available and how to
  // wire secrets. Living under /admin hid it from non-admin devs,
  // which made copy/pasting a `uses:` block guesswork.
  { label: "Plugins", href: "/plugins", icon: Package },
  // Docs live outside the (dashboard) group (no auth required) but
  // surface here for discoverability. The link is intentionally at
  // the bottom of the workspace list so it reads as reference
  // material rather than another operational page.
  { label: "Docs", href: "/docs", icon: BookOpen },
];

// Admin-scoped pages live at /admin/<thing> so the URL reads
// "this is privileged" at a glance. Settings keeps the
// control-plane config bucket (health/webhooks/retention/
// integrations/auth); user management, audit and secrets get
// their own top-level sidebar entries because operators hit
// them often enough that an extra tab click is friction.
const adminNav: NavItem[] = [
  { label: "Settings", href: "/settings", icon: Settings },
  { label: "Users", href: "/admin/users", icon: Users },
  { label: "Groups", href: "/admin/groups", icon: UsersRound },
  { label: "Profiles", href: "/admin/profiles", icon: Cpu },
  { label: "Audit", href: "/admin/audit", icon: ClipboardList },
  { label: "Secrets", href: "/admin/secrets", icon: KeyRound },
];

export function AppSidebar({ user, loginBase }: Props) {
  const pathname = usePathname();

  return (
    <Sidebar collapsible="icon">
      <SidebarHeader>
        <div className="flex items-center gap-2.5 px-2 py-1.5 group-data-[collapsible=icon]:justify-center group-data-[collapsible=icon]:px-0">
          {/* Hex uses foreground (neutral) — chevron pulls the
              brand accent from --primary internally, so the mark
              already has the dual-tone without a prop here. */}
          <Logo size={32} className="text-foreground" />
          {/* Wordmark hides when the sidebar collapses to icon-only
              mode — the mark alone carries the brand at that size. */}
          <Wordmark className="truncate group-data-[collapsible=icon]:hidden" />
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
        {user ? (
          <SidebarUserMenu user={user} loginBase={loginBase ?? ""} />
        ) : (
          <p className="px-2 py-1 text-[10px] uppercase tracking-wide text-muted-foreground group-data-[collapsible=icon]:hidden">
            Control plane
          </p>
        )}
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
