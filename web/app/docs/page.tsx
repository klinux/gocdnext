import Link from "next/link";
import type { Metadata, Route } from "next";
import { ArrowRight } from "lucide-react";

import { listDocs } from "@/server/queries/docs";

export const metadata: Metadata = {
  title: "Docs — gocdnext",
};

// force-dynamic so listDocs() runs at request time. Without it the
// page is statically prerendered at build time, when the docs/
// folder isn't yet present at the resolved path (build cwd ≠
// runtime cwd in the standalone container).
export const dynamic = "force-dynamic";

// Index page: renders a cover shot of what's in the docs folder
// so first-time visitors know where to start. Clicking any tile
// drops them into a single doc.
export default async function DocsIndex() {
  const docs = await listDocs();
  return (
    <div className="space-y-8">
      <header className="space-y-2">
        <h1 className="text-3xl font-semibold tracking-tight">gocdnext docs</h1>
        <p className="text-muted-foreground">
          Architecture, pipeline spec, and internal design notes.
          Pick a section below or use the sidebar.
        </p>
      </header>

      <ul className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        {docs.map((d) => (
          <li key={d.slug}>
            <Link
              href={`/docs/${d.slug}` as Route}
              className="group block rounded-lg border border-border bg-card p-4 transition-colors hover:border-primary/40 hover:bg-muted/40"
            >
              <div className="flex items-center justify-between gap-3">
                <span className="font-semibold">{d.title}</span>
                <ArrowRight
                  className="size-4 text-muted-foreground transition-transform group-hover:translate-x-0.5 group-hover:text-foreground"
                  aria-hidden
                />
              </div>
              <p className="mt-1 font-mono text-[11px] text-muted-foreground">
                /docs/{d.slug}
              </p>
            </Link>
          </li>
        ))}
      </ul>
    </div>
  );
}
