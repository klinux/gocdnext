import Link from "next/link";
import type { Metadata, Route } from "next";
import { ArrowRight } from "lucide-react";

import { docSections, listDocs } from "@/server/queries/docs";

export const metadata: Metadata = {
  title: "Docs — gocdnext",
};

// force-dynamic so listDocs() runs at request time. Without it the page is
// statically prerendered at build time, when the content dir isn't yet present
// at the resolved path (build cwd ≠ runtime cwd in the standalone container).
export const dynamic = "force-dynamic";

// Index page: the same content as the public docs site, grouped by section so
// first-time visitors know where to start. Clicking a tile opens that doc.
export default async function DocsIndex() {
  const sections = docSections(await listDocs());
  return (
    <div className="space-y-8">
      <header className="space-y-2">
        <h1 className="text-3xl font-semibold tracking-tight">gocdnext docs</h1>
        <p className="text-muted-foreground">
          Concepts, the pipeline spec, install &amp; operate guides, and the
          reference. The same content as the public docs site. Pick a section
          below or use the sidebar.
        </p>
      </header>

      {sections.map((section) => (
        <section key={section.group || "overview"} className="space-y-3">
          <h2 className="text-sm font-semibold uppercase tracking-wide text-muted-foreground">
            {section.label}
          </h2>
          <ul className="grid grid-cols-1 gap-3 sm:grid-cols-2">
            {section.docs.map((d) => (
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
        </section>
      ))}
    </div>
  );
}
