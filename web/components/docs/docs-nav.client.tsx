"use client";

import Link from "next/link";
import type { Route } from "next";
import { usePathname } from "next/navigation";

import { cn } from "@/lib/utils";
import type { DocEntry } from "@/server/queries/docs";

type Props = { docs: DocEntry[] };

// DocsNav renders the sidebar list of docs and highlights the
// currently-viewed page via the route's slug. Pulled into a
// client component so the active state is computed from
// pathname (the layout itself is server-rendered).
export function DocsNav({ docs }: Props) {
  const pathname = usePathname();
  return (
    <ul className="space-y-0.5">
      {docs.map((d) => {
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
  );
}
