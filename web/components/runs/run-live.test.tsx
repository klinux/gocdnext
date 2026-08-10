import { describe, expect, it } from "vitest";

import type { LogLine } from "@/types/api";

import { mergeLogMeta } from "./run-live.client";

const head: LogLine[] = [{ seq: 1, stream: "stdout", at: "", text: "start" }];

describe("mergeLogMeta", () => {
  // The core fix: a headless 2s poll (logs=50, no head/omitted) must NOT drop
  // the head + omitted the initial head+tail load carried — otherwise the full
  // run's "Logs (X of Y)" collapses to a tail-only window on the first tick.
  it("preserves prior head/omitted when the poll response is headless", () => {
    const prior = { logs_head: head, logs_omitted: 550, logs_total: 602 };
    const poll = { logs_total: 640 }; // fresh total, no head/omitted
    const merged = mergeLogMeta(poll, prior);
    expect(merged.logs_head).toBe(head); // preserved
    expect(merged.logs_omitted).toBe(550); // preserved
    expect(merged.logs_total).toBe(640); // fresh total wins
  });

  // A fully rich response (initial load / head fetch) replaces everything.
  it("takes everything from a rich response", () => {
    const prior = { logs_head: [], logs_omitted: 0, logs_total: 10 };
    const rich = { logs_head: head, logs_omitted: 60, logs_total: 602 };
    expect(mergeLogMeta(rich, prior)).toEqual({
      logs_head: head,
      logs_omitted: 60,
      logs_total: 602,
    });
  });

  // No prior + poor response → nothing to carry (older server / cold start).
  it("is all-undefined with no prior and a poor response", () => {
    expect(mergeLogMeta({}, undefined)).toEqual({
      logs_head: undefined,
      logs_omitted: undefined,
      logs_total: undefined,
    });
  });
});
