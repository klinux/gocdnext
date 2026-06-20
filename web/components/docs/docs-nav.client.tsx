"use client";

import Link from "next/link";
import type { Route } from "next";
import { usePathname } from "next/navigation";

import { cn } from "@/lib/utils";
import type { DocSection } from "@/server/queries/docs";

type Props = { sections: DocSection[] };

// DocsNav renders the sidebar grouped by section (Concepts, Pipelines, …) and
// highlights the current page from the route slug. Client component so the
// active state reads pathname (the layout itself is server-rendered).
export function DocsNav({ sections }: Props) {
  const pathname = usePathname();
  return (
    <nav className="space-y-4">
      {sections.map((section) => (
        <div key={section.group || "overview"} className="space-y-0.5">
          <p className="px-2 pb-1 text-xs font-semibold uppercase tracking-wide text-muted-foreground/70">
            {section.label}
          </p>
          <ul className="space-y-0.5">
            {section.docs.map((d) => {
              const href = `/docs/${d.slug}` as Route;
              const active = pathname === href;
              return (
                <li key={d.slug}>
                  <Link
                    href={href}
                    className={cn(
                      "block rounded-md px-2 py-1 text-sm transition-colors",
                      active
                        ? "bg-muted font-semibold text-foreground"
                        : "text-muted-foreground hover:bg-muted/60 hover:text-foreground",
                    )}
                  >
                    {d.title}
                  </Link>
                </li>
              );
            })}
          </ul>
        </div>
      ))}
    </nav>
  );
}
