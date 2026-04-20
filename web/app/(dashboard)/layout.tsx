import type { ReactNode } from "react";
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

type Props = { children: ReactNode };

export default function DashboardLayout({ children }: Props) {
  return (
    <QueryClientProvider>
      <SidebarProvider>
        <AppSidebar />
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
