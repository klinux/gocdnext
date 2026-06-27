import { describe, expect, it } from "vitest";

import { POLICY_TEMPLATES } from "./policy-templates";

describe("POLICY_TEMPLATES", () => {
  it("ships a non-empty library with unique keys", () => {
    expect(POLICY_TEMPLATES.length).toBeGreaterThan(0);
    const keys = POLICY_TEMPLATES.map((t) => t.key);
    expect(new Set(keys).size).toBe(keys.length);
  });

  it("every template is a valid compliance fragment (reserved prefix)", () => {
    for (const t of POLICY_TEMPLATES) {
      expect(t.label, t.key).toBeTruthy();
      expect(t.description, t.key).toBeTruthy();
      // stages + job names must carry the _compliance_ prefix the server
      // enforces (ValidatePolicyNames) — a typo here would fail on save.
      const stageMatch = t.configYaml.match(/^stages:\s*\[([^\]]+)\]/m);
      expect(stageMatch, `${t.key} has a stages line`).toBeTruthy();
      for (const stage of stageMatch![1]!.split(",")) {
        expect(stage.trim(), `${t.key} stage`).toMatch(/^_compliance_/);
      }
      // Each job key (two-space indented under jobs:) is prefixed too.
      const jobKeys = [...t.configYaml.matchAll(/^ {2}(\S+):/gm)].map((m) => m[1]!);
      expect(jobKeys.length, `${t.key} declares a job`).toBeGreaterThan(0);
      for (const job of jobKeys) {
        expect(job, `${t.key} job`).toMatch(/^_compliance_/);
      }
    }
  });
});
