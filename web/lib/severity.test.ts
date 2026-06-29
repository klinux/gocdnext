import { describe, expect, it } from "vitest";

import { severityLabel, severityTone, SEVERITY_ORDER } from "./severity";

describe("severityTone", () => {
  it("maps each severity to a design-system tone", () => {
    expect(severityTone("critical")).toBe("failed");
    expect(severityTone("high")).toBe("warning");
    expect(severityTone("medium")).toBe("running");
    expect(severityTone("low")).toBe("queued");
  });
  it("falls back to neutral for unknown", () => {
    expect(severityTone("bogus")).toBe("neutral");
  });
});

describe("severityLabel", () => {
  it("capitalizes; em-dash for empty", () => {
    expect(severityLabel("high")).toBe("High");
    expect(severityLabel("")).toBe("—");
  });
});

describe("SEVERITY_ORDER", () => {
  it("is worst → least", () => {
    expect(SEVERITY_ORDER).toEqual(["critical", "high", "medium", "low"]);
  });
});
