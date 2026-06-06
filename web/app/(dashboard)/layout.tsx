import type { ReactNode } from "react";
import { cookies, headers } from "next/headers";
import { redirect } from "next/navigation";

import { Separator } from "@/components/ui/separator";
import {
  SidebarInset,
  SidebarProvider,
  SidebarTrigger,
} from "@/components/ui/sidebar";
import { AppSidebar } from "@/components/layout/app-sidebar.client";
import { RouteBreadcrumbs } from "@/components/layout/breadcrumbs.client";
import { CommandPalette } from "@/components/layout/command-palette.client";
import { ThemeToggle } from "@/components/layout/theme-toggle.client";
import { QueryClientProvider } from "@/components/providers/query-client-provider.client";
import { Toaster } from "@/components/ui/sonner";
import { resolveAuthState } from "@/server/queries/auth";

type Props = { children: ReactNode };

export default async function DashboardLayout({ children }: Props) {
  const auth = await resolveAuthState();
  if (auth.mode === "anonymous") {
    // Server-side redirect keeps the URL clean — no client-side
    // flash of the dashboard shell before the JS detects 401.
    const hdr = await headers();
    const next = hdr.get("x-pathname") ?? "/";
    redirect(`/login?next=${encodeURIComponent(next)}`);
  }

  const user = auth.mode === "authenticated" ? auth.user : undefined;

  // shadcn's <Sidebar> writes "sidebar_state" cookie on every toggle
  // (components/ui/sidebar.tsx). Read it SSR-side so the rendered
  // markup already matches the user's last choice — without this
  // the layout always boots open and the client only "corrects" on
  // hydration, which (a) flashes the wrong width and (b) only ever
  // wrote, never read, the cookie. Default to open when no cookie
  // yet (first visit).
  const cookieStore = await cookies();
  const sidebarCookie = cookieStore.get("sidebar_state")?.value;
  const defaultSidebarOpen = sidebarCookie !== "false";

  return (
    <QueryClientProvider>
      <SidebarProvider defaultOpen={defaultSidebarOpen}>
        <AppSidebar user={user} />
        <SidebarInset>
          <header className="sticky top-0 z-10 flex h-14 shrink-0 items-center gap-2 border-b bg-background/95 px-4 backdrop-blur supports-[backdrop-filter]:bg-background/80">
            <SidebarTrigger className="-ml-1" />
            <Separator orientation="vertical" className="mr-2 h-4" />
            <RouteBreadcrumbs />
            <div className="ml-auto flex items-center gap-2">
              <CommandPalette />
              <ThemeToggle />
            </div>
          </header>
          <div className="flex-1 p-6 lg:p-8">{children}</div>
        </SidebarInset>
      </SidebarProvider>
      <Toaster position="top-right" richColors />
    </QueryClientProvider>
  );
}
