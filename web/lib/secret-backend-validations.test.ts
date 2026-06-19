import { describe, expect, it } from "vitest";

import { secretBackendWriteSchema } from "./validations";

// The schema enforces the per-source required fields the server also
// checks, so an obvious typo fails before the round-trip. Disabled
// backends skip the field checks entirely (you can save a disabled
// backend with empty config).
describe("secretBackendWriteSchema", () => {
  it("rejects an enabled Vault with no address", () => {
    const res = secretBackendWriteSchema.safeParse({
      source: "vault",
      enabled: true,
      value: { auth: "approle", role_id: "r1" },
    });
    expect(res.success).toBe(false);
    if (!res.success) {
      expect(res.error.issues[0]?.message).toMatch(/Vault address/);
    }
  });

  it("rejects an enabled Vault approle with no role_id", () => {
    const res = secretBackendWriteSchema.safeParse({
      source: "vault",
      enabled: true,
      value: { addr: "https://vault.example.com", auth: "approle" },
    });
    expect(res.success).toBe(false);
    if (!res.success) {
      expect(res.error.issues[0]?.message).toMatch(/role_id/);
    }
  });

  it("accepts a Vault kubernetes auth without role_id", () => {
    const res = secretBackendWriteSchema.safeParse({
      source: "vault",
      enabled: true,
      value: { addr: "https://vault.example.com", auth: "kubernetes", role: "ci" },
    });
    expect(res.success).toBe(true);
  });

  it("rejects an enabled GCP with no project", () => {
    const res = secretBackendWriteSchema.safeParse({
      source: "gcp",
      enabled: true,
      value: {},
    });
    expect(res.success).toBe(false);
    if (!res.success) {
      expect(res.error.issues[0]?.message).toMatch(/GCP project/);
    }
  });

  it("rejects an enabled AWS with no region", () => {
    const res = secretBackendWriteSchema.safeParse({
      source: "aws",
      enabled: true,
      value: {},
    });
    expect(res.success).toBe(false);
    if (!res.success) {
      expect(res.error.issues[0]?.message).toMatch(/AWS region/);
    }
  });

  it("skips field checks when the backend is disabled", () => {
    const res = secretBackendWriteSchema.safeParse({
      source: "vault",
      enabled: false,
      value: {},
    });
    expect(res.success).toBe(true);
  });

  it("accepts a valid enabled GCP backend", () => {
    const res = secretBackendWriteSchema.safeParse({
      source: "gcp",
      enabled: true,
      value: { project: "my-project" },
    });
    expect(res.success).toBe(true);
  });

  it("accepts preserve_credentials and a credentials map on Vault", () => {
    const res = secretBackendWriteSchema.safeParse({
      source: "vault",
      enabled: true,
      value: { addr: "https://vault.example.com", auth: "approle", role_id: "r1" },
      credentials: { secret_id: "s1" },
      preserve_credentials: false,
    });
    expect(res.success).toBe(true);
  });
});
