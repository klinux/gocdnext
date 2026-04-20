"use client";

import { useEffect, useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import type { Route } from "next";
import { ChevronsUpDown, LogOut, Settings, ShieldCheck, User as UserIcon } from "lucide-react";
import { toast } from "sonner";

import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { SidebarMenu, SidebarMenuButton, SidebarMenuItem } from "@/components/ui/sidebar";
import { logoutAction } from "@/server/actions/auth";
import type { CurrentUser } from "@/types/api";

type Props = { user: CurrentUser; loginBase: string };

// SidebarUserMenu lives at the bottom of the AppSidebar. Trigger is
// a SidebarMenuButton so it inherits sidebar layout + collapsing
// behavior (avatar-only in icon mode). Dropdown floats to the right
// + above so it doesn't cover the sidebar content.
export function SidebarUserMenu({ user, loginBase }: Props) {
  const router = useRouter();
  const [pending, startTransition] = useTransition();

  const doLogout = () => {
    startTransition(async () => {
      const res = await logoutAction();
      if (res.ok) {
        router.push("/login" as Route);
        router.refresh();
      } else {
        toast.error(`Logout failed: ${res.error}`);
      }
    });
  };

  const initials = initialsOf(user.name || user.email);
  const display = user.name || user.email;

  return (
    <SidebarMenu>
      <SidebarMenuItem>
        <DropdownMenu>
          <DropdownMenuTrigger
            render={
              <SidebarMenuButton
                size="lg"
                className="data-[state=open]:bg-sidebar-accent data-[state=open]:text-sidebar-accent-foreground"
                aria-label="Account menu"
              >
                <Avatar url={user.avatar_url} initials={initials} />
                <div className="grid flex-1 text-left text-sm leading-tight">
                  <span className="truncate font-medium">{display}</span>
                  <span className="truncate text-xs text-muted-foreground">
                    {user.email}
                  </span>
                </div>
                <ChevronsUpDown className="ml-auto size-4" aria-hidden />
              </SidebarMenuButton>
            }
          />
          <DropdownMenuContent
            side="right"
            align="end"
            sideOffset={8}
            className="w-56"
          >
            <DropdownMenuGroup>
              <DropdownMenuLabel className="flex items-start gap-2">
                <Avatar url={user.avatar_url} initials={initials} />
                <div className="min-w-0">
                  <p className="truncate text-xs font-medium">{display}</p>
                  <p className="truncate text-[10px] text-muted-foreground">
                    {user.email}
                  </p>
                  <p className="mt-0.5 inline-flex items-center gap-1 rounded-sm bg-muted px-1 py-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
                    {user.role === "admin" ? (
                      <ShieldCheck className="size-3" />
                    ) : (
                      <UserIcon className="size-3" />
                    )}
                    {user.role}
                  </p>
                </div>
              </DropdownMenuLabel>
            </DropdownMenuGroup>
            <DropdownMenuSeparator />
            <DropdownMenuGroup>
              <DropdownMenuItem
                onClick={() => router.push("/account" as Route)}
                disabled={pending}
              >
                <Settings className="size-4" />
                Account
              </DropdownMenuItem>
              <DropdownMenuItem onClick={doLogout} disabled={pending}>
                <LogOut className="size-4" />
                Sign out
              </DropdownMenuItem>
            </DropdownMenuGroup>
            <DropdownMenuSeparator />
            <DropdownMenuItem disabled className="text-[10px] text-muted-foreground">
              via {user.provider} · {loginBase}
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </SidebarMenuItem>
    </SidebarMenu>
  );
}

function Avatar({ url, initials }: { url?: string; initials: string }) {
  // Same hydration-safe pattern as the old UserMenu: initials on
  // SSR + first paint, swap to <img> after mount so ad blockers /
  // Dark Reader can't trip React's hydration diff.
  const [mounted, setMounted] = useState(false);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    setMounted(true);
  }, []);

  if (mounted && url && !failed) {
    return (
      <span className="inline-flex size-8 shrink-0 overflow-hidden rounded-md border bg-muted">
        <img
          src={url}
          alt=""
          width={32}
          height={32}
          className="size-full object-cover"
          onError={() => setFailed(true)}
        />
      </span>
    );
  }
  return (
    <span className="inline-flex size-8 shrink-0 items-center justify-center rounded-md bg-primary/15 text-xs font-semibold text-primary">
      {initials}
    </span>
  );
}

function initialsOf(src: string): string {
  const trimmed = src.trim();
  if (!trimmed) return "?";
  const parts = trimmed.split(/\s+/).slice(0, 2);
  return parts.map((p) => p[0]!.toUpperCase()).join("");
}
