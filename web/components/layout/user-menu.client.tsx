"use client";

import { useEffect, useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import type { Route } from "next";
import { LogOut, ShieldCheck, User as UserIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { logoutAction } from "@/server/actions/auth";
import type { CurrentUser } from "@/types/api";

type Props = { user: CurrentUser; loginBase: string };

// UserMenu renders the signed-in user's avatar + a dropdown with
// their role and a logout action. The action hits /auth/logout on
// the control plane, clears the cookie, then bounces back to
// /login.
export function UserMenu({ user, loginBase }: Props) {
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

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button
            variant="ghost"
            size="sm"
            className="h-8 gap-2 px-2"
            aria-label="Account menu"
            disabled={pending}
          >
            <Avatar url={user.avatar_url} initials={initials} />
            <span className="hidden sm:inline max-w-[140px] truncate text-xs">
              {user.name || user.email}
            </span>
          </Button>
        }
      />
      <DropdownMenuContent align="end" className="w-56">
        <DropdownMenuLabel className="flex items-start gap-2">
          <Avatar url={user.avatar_url} initials={initials} />
          <div className="min-w-0">
            <p className="truncate text-xs font-medium">
              {user.name || user.email}
            </p>
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
        <DropdownMenuSeparator />
        <DropdownMenuItem onClick={doLogout} disabled={pending}>
          <LogOut className="size-4" />
          Sign out
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem disabled className="text-[10px] text-muted-foreground">
          via {user.provider} · {loginBase}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function Avatar({ url, initials }: { url?: string; initials: string }) {
  // Render initials on SSR and during the first paint, then swap
  // to the remote <img> after mount. Ad blockers / Dark Reader /
  // other browser extensions routinely mutate <img> tags and trip
  // React's hydration diff — this pattern sidesteps that by never
  // letting the server-rendered tree include the <img> at all.
  const [mounted, setMounted] = useState(false);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    setMounted(true);
  }, []);

  if (mounted && url && !failed) {
    return (
      <span className="inline-flex size-6 shrink-0 overflow-hidden rounded-full border bg-muted">
        <img
          src={url}
          alt=""
          width={24}
          height={24}
          className="size-full object-cover"
          onError={() => setFailed(true)}
        />
      </span>
    );
  }
  return (
    <span className="inline-flex size-6 shrink-0 items-center justify-center rounded-full bg-primary/15 text-[10px] font-semibold text-primary">
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
