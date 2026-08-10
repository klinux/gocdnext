import { describe, expect, it } from "vitest";

import { logWindowCounts } from "./log-window";
import type { LogLine } from "@/types/api";

const tail = (n: number, from = 1): LogLine[] =>
  Array.from({ length: n }, (_, i) => ({
    seq: from + i,
    stream: "stdout",
    at: "",
    text: `line ${from + i}`,
  }));

describe("logWindowCounts", () => {
  // Archived / tail-only: server sends logs_total, NO logs_omitted. The divider
  // count must be derived so the drawer + full run surface "(N omitted)".
  it("derives omitted from logs_total on a headless response", () => {
    const c = logWindowCounts({ logs: tail(5, 596), logs_total: 602 });
    expect(c).toEqual({ shown: 5, total: 602, omitted: 597 });
  });

  // SSE appended lines past the last poll's logs_total → clamp, never X > Y.
  it("clamps a stale total up to the visible count", () => {
    const c = logWindowCounts({ logs: tail(6, 96), logs_total: 5 });
    expect(c).toEqual({ shown: 6, total: 6, omitted: 0 });
  });

  // Older server: no logs_total, fall back to shown + logs_omitted.
  it("falls back to shown + logs_omitted when total is absent", () => {
    const c = logWindowCounts({ logs: tail(5, 96), logs_omitted: 10 });
    expect(c).toEqual({ shown: 5, total: 15, omitted: 10 });
  });

  // Head + tail from a rich load: total wins, omitted is the middle.
  it("uses head + tail with the true total", () => {
    const c = logWindowCounts({
      logs_head: tail(5, 1),
      logs: tail(5, 96),
      logs_total: 100,
    });
    expect(c).toEqual({ shown: 10, total: 100, omitted: 90 });
  });

  // No logs at all → all zero, no divider.
  it("is all-zero for an empty job", () => {
    expect(logWindowCounts({})).toEqual({ shown: 0, total: 0, omitted: 0 });
  });
});
