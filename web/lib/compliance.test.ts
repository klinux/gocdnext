import { describe, expect, it } from "vitest";

import { isComplianceEntry, isCompliancePipeline } from "./compliance";

describe("isComplianceEntry", () => {
  it("flags policy-injected stage/job names", () => {
    expect(isComplianceEntry("_compliance_scan")).toBe(true);
    expect(isComplianceEntry("_compliance_sign")).toBe(true);
  });
  it("leaves repo-authored names alone", () => {
    expect(isComplianceEntry("build")).toBe(false);
    expect(isComplianceEntry("compile")).toBe(false);
    // A name that merely contains, but doesn't start with, the prefix.
    expect(isComplianceEntry("my_compliance_job")).toBe(false);
  });
});

describe("isCompliancePipeline", () => {
  it("flags the synthetic compliance pipeline", () => {
    expect(isCompliancePipeline("_compliance")).toBe(true);
  });
  it("leaves repo pipelines alone", () => {
    expect(isCompliancePipeline("main")).toBe(false);
    expect(isCompliancePipeline("deploy")).toBe(false);
  });
});
