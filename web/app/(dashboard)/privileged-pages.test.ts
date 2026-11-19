import { readFileSync, readdirSync } from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";

// Every /admin/* and /settings/* page reads admin-only endpoints. The section
// layouts redirect non-admins too, but Next renders a layout and its page in
// parallel and reuses layouts across client navigations — so the layout alone
// lets the page fire its 403-ing reads (and skips re-checking after a role
// change). The guard must be the FIRST statement of each page, before any
// read. This test keeps a newly added page from forgetting it.
const GUARDED_DIRS = ["admin", "settings"];

// Opening of the default export, its (possibly multi-line) params and return
// type, then optional comment lines, then the guard as the first statement.
const GUARD_FIRST =
  /export default async function \w+\([\s\S]*?\)(?:\s*:[^{]+)?\s*\{\s*(?:\/\/[^\n]*\n\s*)*await requireAdmin\(\);/;

// Recursive so a nested route (e.g. admin/users/[id]/page.tsx) is covered too.
function leafPages(): string[] {
  return GUARDED_DIRS.flatMap((dir) =>
    readdirSync(path.join(__dirname, dir), { recursive: true, encoding: "utf8" })
      .filter((rel) => path.basename(rel) === "page.tsx")
      // settings/page.tsx is a bare redirect to /settings/health (no reads).
      .filter((rel) => !(dir === "settings" && rel === "page.tsx"))
      .map((rel) => path.join(__dirname, dir, rel)),
  );
}

describe("privileged pages", () => {
  const pages = leafPages();

  it("finds the pages it is meant to guard", () => {
    expect(pages.length).toBeGreaterThanOrEqual(16);
  });

  for (const file of pages) {
    const rel = path.relative(__dirname, file);
    it(`${rel} calls requireAdmin() before any read`, () => {
      expect(readFileSync(file, "utf8")).toMatch(GUARD_FIRST);
    });
  }
});
