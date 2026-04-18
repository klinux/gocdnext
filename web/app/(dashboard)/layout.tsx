import type { ReactNode } from "react";
import Link from "next/link";
import { GitBranch, LayoutDashboard } from "lucide-react";
import { cn } from "@/lib/utils";
import { QueryClientProvider } from "@/components/providers/query-client-provider.client";

type Props = { children: ReactNode };

export default function DashboardLayout({ children }: Props) {
  return (
    <QueryClientProvider>
      <DashboardShell>{children}</DashboardShell>
    </QueryClientProvider>
  );
}

function DashboardShell({ children }: Props) {
  return (
    <div className="min-h-screen grid grid-cols-[220px_1fr]">
      <aside className="border-r border-border bg-sidebar px-4 py-6">
        <div className="flex items-center gap-2 px-2 pb-6">
          <GitBranch className="h-5 w-5 text-primary" aria-hidden />
          <span className="font-semibold tracking-tight">gocdnext</span>
        </div>
        <nav className="space-y-1">
          <NavLink href="/" icon={<LayoutDashboard className="h-4 w-4" />}>
            Projects
          </NavLink>
        </nav>
        <p className="mt-10 px-2 text-xs text-muted-foreground">
          Control plane
          <br />
          <code className="text-[11px]">localhost:8153</code>
        </p>
      </aside>
      <main className="min-w-0">
        <header className="border-b border-border px-8 py-4">
          <h1 className="text-sm font-medium tracking-tight text-muted-foreground">
            Dashboard
          </h1>
        </header>
        <div className="p-8">{children}</div>
      </main>
    </div>
  );
}

function NavLink({
  href,
  icon,
  children,
}: {
  href: "/";
  icon: ReactNode;
  children: ReactNode;
}) {
  return (
    <Link
      href={href}
      className={cn(
        "flex items-center gap-2 rounded-md px-2 py-1.5 text-sm text-foreground/80 transition-colors",
        "hover:bg-sidebar-accent hover:text-sidebar-accent-foreground",
      )}
    >
      {icon}
      {children}
    </Link>
  );
}
