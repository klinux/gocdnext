import { describe, expect, it } from "vitest";

import {
  docSections,
  listDocs,
  parseFrontmatter,
  readDoc,
  rewriteDocLinks,
  transformAsides,
} from "./docs";

describe("parseFrontmatter", () => {
  it("strips the --- block and extracts a bare title", () => {
    const { title, body } = parseFrontmatter("---\ntitle: External secret backends\ndescription: x\n---\n\n# Hi\n");
    expect(title).toBe("External secret backends");
    expect(body.startsWith("---")).toBe(false);
    expect(body).toContain("# Hi");
  });

  it("unquotes a quoted title", () => {
    expect(parseFrontmatter(`---\ntitle: "Quoted: thing"\n---\nbody`).title).toBe("Quoted: thing");
  });

  it("passes through content with no frontmatter", () => {
    const { title, body } = parseFrontmatter("# No frontmatter\n");
    expect(title).toBeUndefined();
    expect(body).toBe("# No frontmatter\n");
  });
});

describe("rewriteDocLinks", () => {
  it("remaps the deployed /gocdnext/docs prefix to /docs and drops the trailing slash", () => {
    expect(rewriteDocLinks("see [clusters](/gocdnext/docs/concepts/clusters/)")).toBe(
      "see [clusters](/docs/concepts/clusters)",
    );
  });

  it("remaps root-relative section links and keeps anchors", () => {
    expect(rewriteDocLinks("[a](/concepts/id-tokens/#x)")).toBe("[a](/docs/concepts/id-tokens#x)");
  });

  it("leaves external links alone", () => {
    const ext = "[gh](https://github.com/klinux/gocdnext)";
    expect(rewriteDocLinks(ext)).toBe(ext);
  });
});

describe("transformAsides", () => {
  it("turns a Starlight aside into a blockquote", () => {
    const out = transformAsides(":::note\nbe careful\n:::");
    expect(out).toContain("> **Note**");
    expect(out).toContain("> be careful");
    expect(out).not.toContain(":::");
  });

  it("uses a custom aside title when present", () => {
    expect(transformAsides(":::caution[Heads up]\nx\n:::")).toContain("> **Heads up**");
  });
});

describe("readDoc traversal guard", () => {
  it("rejects a slug segment with dots/slashes", async () => {
    expect(await readDoc(["..", "secrets"])).toBeNull();
    expect(await readDoc([])).toBeNull();
  });
});

// Integration against the real docs/src/content/docs tree (cwd = web/).
describe("docs content (real tree)", () => {
  it("lists nested docs across sections and skips .mdx", async () => {
    const docs = await listDocs();
    const slugs = docs.map((d) => d.slug);
    expect(slugs).toContain("concepts/external-secrets");
    expect(slugs).toContain("concepts/clusters");
    // index.mdx / reference/api-explorer.mdx must not appear.
    expect(slugs.some((s) => s === "index" || s.endsWith("api-explorer"))).toBe(false);
  });

  it("groups Concepts first", async () => {
    const sections = docSections(await listDocs());
    expect(sections[0]?.group).toBe("concepts");
  });

  it("reads a doc with its frontmatter title, no --- block, no ::: asides", async () => {
    const doc = await readDoc(["concepts", "external-secrets"]);
    expect(doc).not.toBeNull();
    expect(doc!.title).toBe("External secret backends");
    expect(doc!.markdown.startsWith("---")).toBe(false);
    expect(doc!.markdown).not.toContain(":::");
  });
});
