import { describe, expect, it } from "vitest";
import { statusLabel, statusVariant, isTerminalStatus } from "./status";

describe("statusVariant", () => {
  it("maps known statuses to stable badge variants", () => {
    expect(statusVariant("queued")).toBe("secondary");
    expect(statusVariant("running")).toBe("default");
    expect(statusVariant("success")).toBe("success");
    expect(statusVariant("failed")).toBe("destructive");
    expect(statusVariant("canceled")).toBe("outline");
    expect(statusVariant("skipped")).toBe("outline");
    expect(statusVariant("waiting")).toBe("outline");
  });

  it("falls back to outline for unknown statuses", () => {
    expect(statusVariant("mystery")).toBe("outline");
  });
});

describe("statusLabel", () => {
  it("capitalizes the first letter", () => {
    expect(statusLabel("running")).toBe("Running");
    expect(statusLabel("success")).toBe("Success");
  });

  it("returns the raw value when empty", () => {
    expect(statusLabel("")).toBe("");
  });
});

describe("isTerminalStatus", () => {
  it("is true for success, failed, canceled, skipped", () => {
    expect(isTerminalStatus("success")).toBe(true);
    expect(isTerminalStatus("failed")).toBe(true);
    expect(isTerminalStatus("canceled")).toBe(true);
    expect(isTerminalStatus("skipped")).toBe(true);
  });

  it("is false for queued/running/waiting", () => {
    expect(isTerminalStatus("queued")).toBe(false);
    expect(isTerminalStatus("running")).toBe(false);
    expect(isTerminalStatus("waiting")).toBe(false);
  });
});
