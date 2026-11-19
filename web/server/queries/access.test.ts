import { beforeEach, describe, expect, it, vi } from "vitest";

import type { Route } from "next";

import type { AuthState } from "./auth";

const state = vi.hoisted(() => ({ auth: { mode: "unknown" } as AuthState }));

vi.mock("./auth", async (orig) => ({
  ...(await orig<typeof import("./auth")>()),
  resolveAuthState: async () => state.auth,
}));

// redirect() throws in Next; mimic that so the guard's control flow is real.
vi.mock("next/navigation", () => ({
  redirect: (to: string) => {
    throw new Error(`REDIRECT:${to}`);
  },
}));

import { requireAdmin, requireMaintainer } from "./access";

const as = (role: "admin" | "maintainer" | "viewer"): AuthState =>
  ({ mode: "authenticated", user: { role } }) as AuthState;

beforeEach(() => {
  state.auth = { mode: "unknown" };
});

describe("requireAdmin", () => {
  it("lets an admin through", async () => {
    state.auth = as("admin");
    await expect(requireAdmin()).resolves.toBeUndefined();
  });

  it("lets everyone through when auth is disabled", async () => {
    state.auth = { mode: "disabled" };
    await expect(requireAdmin()).resolves.toBeUndefined();
  });

  it.each(["maintainer", "viewer"] as const)("redirects a %s home", async (role) => {
    state.auth = as(role);
    await expect(requireAdmin()).rejects.toThrow("REDIRECT:/");
  });

  it("fails closed on an unknown auth state", async () => {
    await expect(requireAdmin()).rejects.toThrow("REDIRECT:/");
  });
});

describe("requireMaintainer", () => {
  it("lets a maintainer through", async () => {
    state.auth = as("maintainer");
    await expect(requireMaintainer("/projects/acme" as Route)).resolves.toBeUndefined();
  });

  it("redirects a viewer to the given fallback", async () => {
    state.auth = as("viewer");
    await expect(requireMaintainer("/projects/acme" as Route)).rejects.toThrow(
      "REDIRECT:/projects/acme",
    );
  });
});
