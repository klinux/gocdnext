import { promises as fs } from "node:fs";
import path from "node:path";

// DOCS_DIR is the absolute filesystem path to the monorepo's
// `docs/` folder. Built from process.cwd() which is the web/
// dir in dev (`pnpm dev`) and typically in production Docker
// images. If the docs folder isn't where expected, listDocs
// returns an empty list rather than crashing — docs are a
// nice-to-have, not load-bearing for the app.
const DOCS_DIR = path.resolve(process.cwd(), "..", "docs");

export type DocEntry = {
  // slug matches the filename without the .md extension.
  // Used as the URL segment: /docs/<slug>.
  slug: string;
  title: string;
  // `order` is the position in the sidebar. Derived from a
  // hand-picked list below so the reading flow (architecture
  // first, spec second, …) beats alphabetical ordering on a
  // grab-bag folder.
  order: number;
};

// Fixed reading order. Files not listed here get pushed to the
// end alphabetically — so a newly-added doc shows up without
// code changes, but the curated flow stays stable for the ones
// we've thought through.
const ORDER: string[] = [
  "architecture",
  "pipeline-spec",
  "templates",
  "artifacts-design",
  "design-system",
  "cloud-dev",
  "roadmap",
];

// Human-friendly titles per slug. Fallback is a best-effort
// Title Case of the slug itself.
const TITLES: Record<string, string> = {
  architecture: "Architecture",
  "pipeline-spec": "Pipeline spec",
  templates: "Pipeline templates",
  "artifacts-design": "Artifacts design",
  "design-system": "Design system",
  "cloud-dev": "Cloud dev environment",
  roadmap: "Roadmap",
};

function titleFor(slug: string): string {
  if (TITLES[slug]) return TITLES[slug]!;
  return slug
    .split("-")
    .map((s) => s.charAt(0).toUpperCase() + s.slice(1))
    .join(" ");
}

export async function listDocs(): Promise<DocEntry[]> {
  let files: string[];
  try {
    files = await fs.readdir(DOCS_DIR);
  } catch {
    return [];
  }
  const slugs = files
    .filter((f) => f.endsWith(".md"))
    .map((f) => f.slice(0, -3));

  return slugs
    .map<DocEntry>((slug) => ({
      slug,
      title: titleFor(slug),
      order: ORDER.indexOf(slug) === -1 ? 999 : ORDER.indexOf(slug),
    }))
    .sort((a, b) => {
      if (a.order !== b.order) return a.order - b.order;
      return a.title.localeCompare(b.title);
    });
}

export async function readDoc(slug: string): Promise<{
  title: string;
  markdown: string;
} | null> {
  // Defensive against path traversal — the filename is bounded
  // to [A-Za-z0-9_-], rejecting slashes and dots entirely so a
  // slug like "../../etc/passwd" can't escape DOCS_DIR.
  if (!/^[A-Za-z0-9_-]+$/.test(slug)) return null;
  const filepath = path.join(DOCS_DIR, `${slug}.md`);
  try {
    const markdown = await fs.readFile(filepath, "utf8");
    return { title: titleFor(slug), markdown };
  } catch {
    return null;
  }
}
