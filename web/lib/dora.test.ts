import { describe, expect, it } from "vitest";

import {
  cfrTier,
  fmtDuration,
  fmtFreq,
  fmtPct,
  freqTier,
  leadTier,
  mttrTier,
  overallTier,
  ppDelta,
  pctDelta,
} from "./dora";

describe("tier classification", () => {
  it("classifies deployment frequency by cadence", () => {
    expect(freqTier(1.9)).toBe("elite"); // multiple/day
    expect(freqTier(0.5)).toBe("high"); // ~weekly..daily
    expect(freqTier(1 / 20)).toBe("medium"); // ~monthly
    expect(freqTier(1 / 60)).toBe("low");
  });

  it("classifies lead time / MTTR by duration bands", () => {
    expect(leadTier(3600)).toBe("elite"); // < 1 day
    expect(leadTier(3 * 86400)).toBe("high"); // < 1 week
    expect(leadTier(20 * 86400)).toBe("medium"); // < 1 month
    expect(leadTier(60 * 86400)).toBe("low");
    expect(mttrTier(1800)).toBe("elite"); // < 1 hour
    expect(mttrTier(5 * 3600)).toBe("high"); // < 1 day
  });

  it("classifies change failure rate by threshold", () => {
    expect(cfrTier(0.1)).toBe("elite");
    expect(cfrTier(0.25)).toBe("high");
    expect(cfrTier(0.4)).toBe("medium");
    expect(cfrTier(0.67)).toBe("low");
  });

  it("derives overall tier from the weakest metric (you're only as good as your worst)", () => {
    expect(overallTier(["elite", "high", "high", "high"])).toBe("high");
    expect(overallTier(["elite", "elite", "high", "high"])).toBe("high");
    expect(overallTier(["elite", "elite", "elite", "low"])).toBe("low"); // one catastrophe drags it down
    expect(overallTier(["elite", "elite", "elite", "elite"])).toBe("elite");
  });
});

describe("formatters", () => {
  it("formats durations like the handoff", () => {
    expect(fmtDuration(120)).toBe("2m");
    expect(fmtDuration(48 * 60)).toBe("48m");
    expect(fmtDuration(3 * 3600 + 12 * 60)).toBe("3h 12m");
    expect(fmtDuration(18 * 3600)).toBe("18h");
    expect(fmtDuration(5 * 86400)).toBe("5 dias");
    expect(fmtDuration(0)).toBe("—");
  });

  it("formats frequency per day or per week", () => {
    expect(fmtFreq(1.9)).toBe("1.9/dia");
    expect(fmtFreq(3.3 / 7, "sem")).toBe("3.3/sem");
    expect(fmtFreq(1, "dia")).toBe("1/dia");
  });

  it("formats a rate as whole percent", () => {
    expect(fmtPct(0.24)).toBe("24%");
  });
});

describe("deltas with semantic goodness", () => {
  it("treats a frequency rise as good", () => {
    const d = pctDelta(1.9, 1.6, false);
    expect(d.text).toBe("+19%");
    expect(d.good).toBe(true);
  });

  it("treats a lead-time fall as good", () => {
    const d = pctDelta(3.2, 4.2, true);
    expect(d.text.startsWith("−")).toBe(true);
    expect(d.good).toBe(true);
  });

  it("treats a CFR rise (pp) as bad", () => {
    const d = ppDelta(0.24, 0.18);
    expect(d.text).toBe("+6pp");
    expect(d.good).toBe(false);
  });

  it("is flat without a prior baseline", () => {
    expect(pctDelta(5, 0, true).good).toBeNull();
    expect(ppDelta(0.2, 0.2).text).toBe("—");
  });
});
