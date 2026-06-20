import { promises as fs, type Dirent } from "node:fs";
import path from "node:path";

// The in-app /docs page serves the SAME markdown as the public docs site
// (Starlight) — single source of truth, so it can't drift. Content lives at
// docs/src/content/docs/**; the path is resolved lazily (process.cwd() at
// request time, not module load) so the standalone runtime picks it up.
//
// Layout:
//   - dev (`pnpm dev` from web/): cwd=/repo/web → /repo/docs/src/content/docs
//   - prod standalone: cwd=/app, Dockerfile COPYs docs/src/content/docs to
//     /docs/src/content/docs (sibling of /app) → same relative path resolves.
//
// Missing dir is non-fatal: listDocs returns [] rather than crash.
function docsDir(): string {
  return path.resolve(process.cwd(), "..", "docs", "src", "content", "docs");
}

export type DocEntry = {
  // slug is the posix path under the content root, sans extension —
  // the URL segments for /docs/<slug> (e.g. "concepts/clusters").
  slug: string;
  title: string;
  // group is the top-level section (concepts | pipelines | install |
  // reference | "") used to bucket the sidebar + index.
  group: string;
  order: number;
};

export type DocSection = { group: string; label: string; docs: DocEntry[] };

// Section order + labels for the sidebar/index. Groups not listed fall to the
// end, alphabetically.
const GROUP_ORDER = ["concepts", "pipelines", "install", "reference"];
const GROUP_LABELS: Record<string, string> = {
  concepts: "Concepts",
  pipelines: "Pipelines",
  install: "Install & operate",
  reference: "Reference",
  "": "Overview",
};

// Curated reading order for a few high-traffic slugs; everything else sorts
// alphabetically by title within its group.
const SLUG_ORDER: string[] = [
  "concepts/architecture",
  "concepts/vsm",
  "concepts/materials",
  "concepts/secrets",
  "concepts/external-secrets",
  "pipelines/quickstart",
  "pipelines/yaml-reference",
  "install/helm",
  "install/auth",
  "reference/cli",
  "reference/plugins",
];

function groupLabel(group: string): string {
  return GROUP_LABELS[group] ?? group.charAt(0).toUpperCase() + group.slice(1);
}

// walkMarkdown returns every .md file under dir (recursively), as posix paths
// relative to dir, sans extension. .mdx is skipped — those use Starlight/Astro
// components the in-app renderer can't render.
async function walkMarkdown(dir: string, base = ""): Promise<string[]> {
  let entries: Dirent[];
  try {
    entries = await fs.readdir(dir, { withFileTypes: true });
  } catch {
    return [];
  }
  const out: string[] = [];
  for (const e of entries) {
    const rel = base ? `${base}/${e.name}` : e.name;
    if (e.isDirectory()) {
      out.push(...(await walkMarkdown(path.join(dir, e.name), rel)));
    } else if (e.name.endsWith(".md")) {
      out.push(rel.slice(0, -3));
    }
  }
  return out;
}

// parseFrontmatter splits a leading `---` YAML block from the body and pulls
// out `title`. We don't need a full YAML parser — titles are scalar strings.
export function parseFrontmatter(raw: string): { title?: string; body: string } {
  if (!raw.startsWith("---\n")) return { body: raw };
  const end = raw.indexOf("\n---", 4);
  if (end === -1) return { body: raw };
  const block = raw.slice(4, end);
  const body = raw.slice(end + 4).replace(/^\r?\n/, "");
  const m = block.match(/^title:\s*(.+)$/m);
  let title: string | undefined;
  if (m) {
    title = m[1]!.trim().replace(/^["']|["']$/g, "");
  }
  return { title, body };
}

// rewriteDocLinks remaps the Starlight site's internal links to the in-app
// /docs base: the deployed `/gocdnext/docs/...` prefix and root-relative
// `/concepts|pipelines|install|reference/...` links both become `/docs/...`,
// and a Starlight trailing slash before a `)` or `#` is dropped (the in-app
// catch-all route matches no trailing slash). External (http) links untouched.
export function rewriteDocLinks(body: string): string {
  let out = body.replaceAll("](/gocdnext/docs/", "](/docs/");
  // Root-relative section links → /docs/… (skip ones already under /docs/).
  out = out.replace(/\]\(\/(concepts|pipelines|install|reference)\//g, "](/docs/$1/");
  // Drop a trailing slash inside a /docs link before ) or #anchor.
  out = out.replace(/(\]\(\/docs\/[^)#\s]*?)\/(\)|#)/g, "$1$2");
  return out;
}

// transformAsides turns Starlight's `:::note|tip|caution|danger[title]` blocks
// into blockquotes the plain markdown renderer can show (rare — a couple of
// files use them).
export function transformAsides(body: string): string {
  return body.replace(
    /^:::(note|tip|caution|danger)(?:\[([^\]]*)\])?[ \t]*\r?\n([\s\S]*?)\r?\n:::[ \t]*$/gm,
    (_all, kind: string, title: string | undefined, inner: string) => {
      const heading = (title && title.trim()) || kind.charAt(0).toUpperCase() + kind.slice(1);
      const quoted = inner
        .split("\n")
        .map((line) => (line ? `> ${line}` : ">"))
        .join("\n");
      return `> **${heading}**\n>\n${quoted}`;
    },
  );
}

export async function listDocs(): Promise<DocEntry[]> {
  const slugs = await walkMarkdown(docsDir());
  const entries = await Promise.all(
    slugs.map(async (slug): Promise<DocEntry> => {
      const group = slug.includes("/") ? slug.split("/")[0]! : "";
      let title = slugTitle(slug);
      try {
        const raw = await fs.readFile(path.join(docsDir(), `${slug}.md`), "utf8");
        const fm = parseFrontmatter(raw);
        if (fm.title) title = fm.title;
      } catch {
        // keep the derived title
      }
      const order = SLUG_ORDER.indexOf(slug);
      return { slug, title, group, order: order === -1 ? 999 : order };
    }),
  );
  return entries.sort(compareEntries);
}

// docSections buckets entries into ordered sidebar sections.
export function docSections(entries: DocEntry[]): DocSection[] {
  const byGroup = new Map<string, DocEntry[]>();
  for (const e of entries) {
    const list = byGroup.get(e.group) ?? [];
    list.push(e);
    byGroup.set(e.group, list);
  }
  return [...byGroup.keys()]
    .sort((a, b) => {
      const ia = GROUP_ORDER.indexOf(a);
      const ib = GROUP_ORDER.indexOf(b);
      return (ia === -1 ? 99 : ia) - (ib === -1 ? 99 : ib) || a.localeCompare(b);
    })
    .map((group) => ({
      group,
      label: groupLabel(group),
      docs: (byGroup.get(group) ?? []).slice().sort(compareEntries),
    }));
}

export async function readDoc(slugParts: string[]): Promise<{
  title: string;
  markdown: string;
} | null> {
  // Defensive against traversal — every segment is bounded to [A-Za-z0-9_-],
  // rejecting slashes/dots so a crafted slug can't escape docsDir().
  if (slugParts.length === 0 || !slugParts.every((s) => /^[A-Za-z0-9_-]+$/.test(s))) {
    return null;
  }
  const slug = slugParts.join("/");
  try {
    const raw = await fs.readFile(path.join(docsDir(), `${slug}.md`), "utf8");
    const fm = parseFrontmatter(raw);
    const markdown = transformAsides(rewriteDocLinks(fm.body));
    return { title: fm.title ?? slugTitle(slug), markdown };
  } catch {
    return null;
  }
}

function slugTitle(slug: string): string {
  const last = slug.includes("/") ? slug.slice(slug.lastIndexOf("/") + 1) : slug;
  return last
    .split("-")
    .map((s) => s.charAt(0).toUpperCase() + s.slice(1))
    .join(" ");
}

function compareEntries(a: DocEntry, b: DocEntry): number {
  if (a.order !== b.order) return a.order - b.order;
  return a.title.localeCompare(b.title);
}
