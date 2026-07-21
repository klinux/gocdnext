import { describe, expect, it } from "vitest";

import {
  analysisTone,
  imageParts,
  isManualGate,
  shortHash,
  statusFor,
  stepLabel,
  stepState,
  trafficSplit,
} from "./rollouts";
import type { RolloutStep } from "@/types/api";

function step(partial: Partial<RolloutStep>): RolloutStep {
  return { kind: "setWeight", weight: null, pause_duration: "", ...partial };
}

describe("statusFor", () => {
  it("lets aborted win over the reported phase", () => {
    // Controller may still say Degraded while traffic snapped back to stable.
    expect(statusFor("Degraded", true)).toEqual({ label: "Aborted", tone: "red" });
    expect(statusFor("Paused", true).label).toBe("Aborted");
  });

  it("maps each phase to its tone", () => {
    expect(statusFor("Paused", false)).toEqual({ label: "Paused", tone: "amber" });
    expect(statusFor("Progressing", false)).toEqual({
      label: "Progressing",
      tone: "teal",
    });
    expect(statusFor("Healthy", false)).toEqual({ label: "Healthy", tone: "green" });
    expect(statusFor("Degraded", false)).toEqual({ label: "Degraded", tone: "red" });
  });

  it("falls back to a neutral tone for an unknown/empty phase", () => {
    expect(statusFor("Mystery", false)).toEqual({ label: "Mystery", tone: "neutral" });
    expect(statusFor("", false)).toEqual({ label: "Unknown", tone: "neutral" });
  });
});

describe("stepState", () => {
  it("marks done/current/pending around a known current index", () => {
    expect(stepState(0, 3, true, false)).toBe("done");
    expect(stepState(2, 3, true, false)).toBe("done");
    expect(stepState(3, 3, true, false)).toBe("current");
    expect(stepState(4, 3, true, false)).toBe("pending");
  });

  it("reads every node as pending when the current index is unknown", () => {
    expect(stepState(0, 0, false, false)).toBe("pending");
    expect(stepState(3, 5, false, false)).toBe("pending");
  });

  it("reads every node as pending when aborted (no progress to trust)", () => {
    expect(stepState(0, 4, true, true)).toBe("pending");
    expect(stepState(4, 4, true, true)).toBe("pending");
  });

  it("marks every node done once the index runs past the last step", () => {
    // Fully promoted: currentIndex == steps.length → nothing is "current".
    expect(stepState(0, 8, true, false)).toBe("done");
    expect(stepState(7, 8, true, false)).toBe("done");
  });
});

describe("isManualGate", () => {
  it("is true only for an indefinite pause", () => {
    expect(isManualGate(step({ kind: "pause", pause_duration: "" }))).toBe(true);
    expect(isManualGate(step({ kind: "pause", pause_duration: "5m" }))).toBe(false);
    expect(isManualGate(step({ kind: "setWeight", weight: 20 }))).toBe(false);
  });
});

describe("stepLabel", () => {
  it("renders setWeight/setCanaryScale with the weight", () => {
    expect(stepLabel(step({ kind: "setWeight", weight: 20 }))).toBe("20%");
    expect(stepLabel(step({ kind: "setCanaryScale", weight: 60 }))).toBe("scale 60%");
  });

  it("distinguishes a timed pause from a manual gate", () => {
    expect(stepLabel(step({ kind: "pause", pause_duration: "5m" }))).toBe("5m");
    expect(stepLabel(step({ kind: "pause", pause_duration: "" }))).toBe("manual");
  });

  it("labels analysis/experiment/plugin and falls back to the kind", () => {
    expect(stepLabel(step({ kind: "analysis" }))).toBe("analysis");
    expect(stepLabel(step({ kind: "experiment" }))).toBe("experiment");
    expect(stepLabel(step({ kind: "plugin" }))).toBe("plugin");
    expect(stepLabel(step({ kind: "other" }))).toBe("other");
  });

  it("degrades gracefully when a weighted step carries no weight", () => {
    expect(stepLabel(step({ kind: "setWeight", weight: null }))).toBe("setWeight");
  });
});

describe("trafficSplit", () => {
  it("derives the stable share from the canary weight", () => {
    expect(trafficSplit(40)).toEqual({ canary: 40, stable: 60 });
    expect(trafficSplit(0)).toEqual({ canary: 0, stable: 100 });
    expect(trafficSplit(100)).toEqual({ canary: 100, stable: 0 });
  });

  it("clamps out-of-range weights", () => {
    expect(trafficSplit(150)).toEqual({ canary: 100, stable: 0 });
    expect(trafficSplit(-10)).toEqual({ canary: 0, stable: 100 });
  });

  it("rounds fractional weights and treats NaN as 0% canary", () => {
    expect(trafficSplit(33.6)).toEqual({ canary: 34, stable: 66 });
    expect(trafficSplit(Number.NaN)).toEqual({ canary: 0, stable: 100 });
  });
});

describe("analysisTone", () => {
  it("maps analysis phases to tones", () => {
    expect(analysisTone("Failed")).toBe("red");
    expect(analysisTone("Error")).toBe("red");
    expect(analysisTone("Inconclusive")).toBe("amber");
    expect(analysisTone("Successful")).toBe("green");
    expect(analysisTone("Running")).toBe("teal");
  });
});

describe("shortHash", () => {
  it("trims a long hash and leaves a short one untouched", () => {
    expect(shortHash("6b5f7c9d8e2a1b")).toBe("6b5f7c9d8e");
    expect(shortHash("abc")).toBe("abc");
  });
});

describe("imageParts", () => {
  it("splits repo/name:tag on the tag separator", () => {
    expect(imageParts("registry/team/checkout-api:1.9.0")).toEqual({
      name: "registry/team/checkout-api",
      tag: "1.9.0",
    });
  });

  it("keeps a tagless image whole", () => {
    expect(imageParts("checkout-api")).toEqual({ name: "checkout-api" });
  });

  it("never splits a digest ref on the sha256 colon", () => {
    const digest = "checkout-api@sha256:deadbeef";
    expect(imageParts(digest)).toEqual({ name: digest });
  });

  it("returns an empty name for an empty image", () => {
    expect(imageParts("")).toEqual({ name: "" });
  });
});
