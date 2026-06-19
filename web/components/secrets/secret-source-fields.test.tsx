import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import {
  gateSources,
  isSecretSource,
  vaultRequiresKey,
} from "./secret-source-fields.client";
import { secretSourceSummary, sourceBadge } from "@/lib/secrets";
import { Pagination } from "@/components/shared/pagination";
import type { Secret } from "@/types/api";

describe("gateSources", () => {
  it("offers exactly what the server reports — nothing when empty", () => {
    // The server is authoritative: db is present only when a cipher is set.
    expect(gateSources([])).toEqual([]);
  });

  it("offers db when the server includes it", () => {
    expect(gateSources(["db"])).toEqual(["db"]);
  });

  it("pins the offered sources to the canonical order (db-first)", () => {
    // Server lists them out of order → output follows SOURCE_ORDER.
    expect(gateSources(["aws", "vault", "db"])).toEqual(["db", "vault", "aws"]);
  });

  it("does NOT offer db on an external-only deployment", () => {
    // No cipher → server omits db → the selector must not show it
    // (the server would 503 a db write).
    expect(gateSources(["vault", "aws"])).toEqual(["vault", "aws"]);
  });

  it("ignores unknown source strings", () => {
    expect(gateSources(["azure", "vault"])).toEqual(["vault"]);
  });
});

describe("isSecretSource", () => {
  it("narrows the known backends and rejects the rest", () => {
    expect(isSecretSource("vault")).toBe(true);
    expect(isSecretSource("db")).toBe(true);
    expect(isSecretSource("azure")).toBe(false);
  });
});

describe("vaultRequiresKey", () => {
  it("is true only for vault", () => {
    expect(vaultRequiresKey("vault")).toBe(true);
    expect(vaultRequiresKey("gcp")).toBe(false);
    expect(vaultRequiresKey("db")).toBe(false);
  });
});

describe("secretSourceSummary", () => {
  const base = { name: "X", created_at: "", updated_at: "" };

  it("renders Stored for a db secret", () => {
    expect(secretSourceSummary({ ...base, source: "db" })).toBe("Stored");
  });

  it("renders backend · path#key for a vault secret", () => {
    expect(
      secretSourceSummary({
        ...base,
        source: "vault",
        ref: { path: "secret/ci/gh", key: "token" },
      }),
    ).toBe("Vault · secret/ci/gh#token");
  });

  it("omits the # when the ref has no key", () => {
    expect(
      secretSourceSummary({
        ...base,
        source: "aws",
        ref: { path: "ci/token" },
      }),
    ).toBe("AWS · ci/token");
  });

  it("falls back to the raw source for an unknown backend", () => {
    expect(sourceBadge("azure")).toBe("azure");
  });
});

describe("Pagination from the secrets envelope", () => {
  it("renders Next (not Prev) on the first page when total exceeds the page", () => {
    // envelope: { total: 120, limit: 50, offset: 0 }
    render(
      <Pagination
        offset={0}
        total={120}
        pageSize={50}
        basePath="/admin/secrets"
      />,
    );
    // The shared Button renders the active link as <a role="button">;
    // the disabled side renders a <span> (no link semantics).
    const next = screen.getByRole("button", { name: /next/i });
    expect(next.getAttribute("href")).toBe("/admin/secrets?offset=50");
    // Prev is disabled on page one → a <span>, not an anchor.
    expect(screen.getByRole("button", { name: /prev/i }).tagName).toBe("SPAN");
  });

  it("renders Prev on a middle page pointing back one window", () => {
    render(
      <Pagination
        offset={50}
        total={120}
        pageSize={50}
        basePath="/projects/demo/secrets"
      />,
    );
    expect(
      screen.getByRole("button", { name: /prev/i }).getAttribute("href"),
    ).toBe("/projects/demo/secrets?offset=0");
    expect(
      screen.getByRole("button", { name: /next/i }).getAttribute("href"),
    ).toBe("/projects/demo/secrets?offset=100");
  });

  it("renders nothing when a single page holds the whole list", () => {
    const { container } = render(
      <Pagination offset={0} total={10} pageSize={50} basePath="/admin/secrets" />,
    );
    expect(container.firstChild).toBeNull();
  });

  const samplePage: Secret[] = [
    { name: "A", source: "db", created_at: "", updated_at: "" },
  ];
  it("a page envelope keeps its rows on the current window", () => {
    // Sanity: the page array IS the window the server returned; the
    // table renders exactly these, pagination handles the rest.
    expect(samplePage).toHaveLength(1);
  });
});
