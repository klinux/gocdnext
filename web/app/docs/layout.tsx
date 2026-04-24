import Link from "next/link";
import type { ReactNode } from "react";
import { BookOpen } from "lucide-react";

import { listDocs } from "@/server/queries/docs";
import { Logo, Wordmark } from "@/components/brand/logo";
import { DocsNav } from "@/components/docs/docs-nav.client";

// Docs live outside the dashboard on purpose — they're public
// reading material (architecture, pipeline spec, design system,
// etc.) that a drive-by visitor or a fresh hire should be able
// to browse without signing in. The layout deliberately doesn't
// pull session state; auth wiring stays in (dashboard).
export default async function DocsLayout({
  children,
}: {
  children: ReactNode;
}) {
  const docs = await listDocs();
  return (
    <div className="min-h-screen bg-background">
      <header className="sticky top-0 z-20 flex h-14 items-center gap-3 border-b border-border bg-background/80 px-6 backdrop-blur">
        <Link href="/" className="flex items-center gap-2.5">
          <Logo size={28} className="text-foreground" />
          <Wordmark />
        </Link>
        <span className="ml-1 rounded-md border border-border bg-muted/40 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
          docs
        </span>
        <div className="ml-auto text-xs text-muted-foreground">
          <Link href="/" className="hover:text-foreground">
            Back to app
          </Link>
        </div>
      </header>

      <div className="mx-auto flex w-full max-w-6xl gap-8 px-6 py-8">
        <aside className="hidden w-56 shrink-0 lg:block">
          <nav aria-label="Docs navigation" className="sticky top-20 space-y-1">
            <h2 className="mb-2 flex items-center gap-2 text-xs font-semibold uppercase tracking-wider text-muted-foreground">
              <BookOpen className="size-3.5" aria-hidden />
              Contents
            </h2>
            <DocsNav docs={docs} />
          </nav>
        </aside>

        <main className="min-w-0 flex-1">{children}</main>
      </div>
    </div>
  );
}
