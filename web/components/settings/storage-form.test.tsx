import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import type { StorageConfig } from "@/server/queries/admin";
import { StorageForm } from "./storage-form.client";

// Server actions are dispatched on save/clear; a render test must never fire a
// real fetch.
vi.mock("@/server/actions/storage", () => ({
  saveStorageConfig: vi.fn(),
  clearStorageConfig: vi.fn(),
}));

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

function cfg(over: Partial<StorageConfig>): StorageConfig {
  return { backend: "s3", value: {}, credential_keys: [], source: "db", ...over };
}

describe("StorageForm", () => {
  // Regression: the server returns credential_keys as JSON null for a Go nil
  // []string (a DB override with no credential blob). The form read
  // `credential_keys.length` during render, which crashed on null.
  it("renders without crashing when credential_keys is null", () => {
    render(<StorageForm initial={cfg({ credential_keys: null })} />);
    expect(screen.getByText("Active configuration")).toBeTruthy();
  });

  it("renders with a populated credential_keys list", () => {
    render(<StorageForm initial={cfg({ credential_keys: ["access_key", "secret_key"] })} />);
    expect(screen.getByText("Active configuration")).toBeTruthy();
  });
});
