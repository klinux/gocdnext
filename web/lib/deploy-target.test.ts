import { describe, expect, it } from "vitest";

import {
  ENVIRONMENT_NAME_RE,
  deployTargetFormSchema,
} from "@/lib/deploy-target";

const valid = {
  environment: "production",
  cluster: "argocd-hub",
  application: "shop-prod",
  namespace: "argocd",
  sync_mode: "trigger" as const,
};

describe("deployTargetFormSchema", () => {
  it("accepts a well-formed target", () => {
    expect(deployTargetFormSchema.safeParse(valid).success).toBe(true);
  });

  it("namespace is optional (server defaults to argocd)", () => {
    const { namespace, ...withoutNs } = valid;
    void namespace;
    expect(deployTargetFormSchema.safeParse(withoutNs).success).toBe(true);
  });

  it("requires a non-empty cluster and application", () => {
    expect(
      deployTargetFormSchema.safeParse({ ...valid, cluster: "  " }).success,
    ).toBe(false);
    expect(
      deployTargetFormSchema.safeParse({ ...valid, application: "" }).success,
    ).toBe(false);
  });

  it("only allows trigger|observe sync modes", () => {
    expect(
      deployTargetFormSchema.safeParse({ ...valid, sync_mode: "trigger" })
        .success,
    ).toBe(true);
    expect(
      deployTargetFormSchema.safeParse({ ...valid, sync_mode: "observe" })
        .success,
    ).toBe(true);
    expect(
      deployTargetFormSchema.safeParse({ ...valid, sync_mode: "auto" }).success,
    ).toBe(false);
  });

  describe("environment name", () => {
    it("accepts the shapes a pipeline could reference", () => {
      for (const e of ["prod", "staging-eu", "a", "env.1", "A0_b-c"]) {
        expect(ENVIRONMENT_NAME_RE.test(e)).toBe(true);
      }
    });

    it("rejects empty, leading punctuation, spaces, slashes, and overlong", () => {
      for (const e of ["", "-prod", ".x", "has space", "a/b", "A".repeat(65)]) {
        expect(ENVIRONMENT_NAME_RE.test(e)).toBe(false);
      }
    });

    it("caps the name at 64 characters", () => {
      expect(ENVIRONMENT_NAME_RE.test("a".repeat(64))).toBe(true);
      expect(ENVIRONMENT_NAME_RE.test("a".repeat(65))).toBe(false);
    });
  });
});
