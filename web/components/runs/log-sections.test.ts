import { describe, expect, it } from "vitest";

import {
  buildLogBlocks,
  defaultCollapsedIds,
  sectionIdForSeq,
  type LogSection,
} from "./log-sections";
import type { LogLine } from "@/types/api";

const ln = (seq: number, text: string, at = `2026-06-12T10:00:${String(seq).padStart(2, "0")}Z`, stream = "stdout"): LogLine => ({
  seq,
  stream,
  at,
  text,
});

// Pull the sections out of the block list for terse assertions.
const sectionsOf = (blocks: ReturnType<typeof buildLogBlocks>): LogSection[] =>
  blocks.flatMap((b) => (b.kind === "section" ? [b.section] : []));

describe("buildLogBlocks", () => {
  it("returns one non-foldable plain section when there are no markers", () => {
    const logs = [ln(1, "$ npm test"), ln(2, "ok")];
    const secs = sectionsOf(buildLogBlocks([], logs, 0));
    expect(secs).toHaveLength(1);
    expect(secs[0]?.status).toBe("plain");
    expect(secs[0]?.label).toBe("");
    expect(secs[0]?.foldable).toBe(false);
    expect(secs[0]?.lines.map((l) => l.seq)).toEqual([1, 2]);
  });

  it("brackets a timedPhase open/close pair into one success section, absorbing the markers", () => {
    const logs = [
      ln(1, "── artifact upload …", "2026-06-12T10:00:00Z"),
      ln(2, "artifact uploaded: dist.tar (12 bytes)"),
      ln(3, "── artifact upload done in 3s", "2026-06-12T10:00:03Z"),
    ];
    const secs = sectionsOf(buildLogBlocks([], logs, 0));
    expect(secs).toHaveLength(1);
    expect(secs[0]?.label).toBe("artifact upload");
    expect(secs[0]?.status).toBe("success");
    expect(secs[0]?.durationSec).toBe(3);
    // markers absorbed → only the real output line remains in the body
    expect(secs[0]?.lines.map((l) => l.seq)).toEqual([2]);
    expect(secs[0]?.foldable).toBe(true);
  });

  it("classifies a `failed after` close as a failed section and keeps the detail", () => {
    const logs = [
      ln(1, "── tasks …", "2026-06-12T10:00:00Z"),
      ln(2, "running suite"),
      ln(3, "── tasks failed after 8s (task 2, exit 1)", "2026-06-12T10:00:08Z"),
    ];
    const sec = sectionsOf(buildLogBlocks([], logs, 0))[0];
    expect(sec?.status).toBe("failed");
    expect(sec?.label).toContain("tasks");
    expect(sec?.label).toContain("exit 1");
    expect(sec?.durationSec).toBe(8);
  });

  it("treats a trailing-summary singleton (no open) as a section over the preceding lines", () => {
    const logs = [
      ln(1, "$ go test ./..."),
      ln(2, "ok pkg/cart"),
      ln(3, "── task completed in 5s", "2026-06-12T10:00:05Z"),
    ];
    const sec = sectionsOf(buildLogBlocks([], logs, 0))[0];
    expect(sec?.label).toBe("task");
    expect(sec?.status).toBe("success");
    expect(sec?.lines.map((l) => l.seq)).toEqual([1, 2]);
    // singleton duration: first body line → summary marker
    expect(sec?.durationSec).toBe(5);
  });

  it("labels lines before the first marker as `job setup` (foldable, open)", () => {
    const logs = [
      ln(1, "cloning repo"),
      ln(2, "── task completed in 2s", "2026-06-12T10:00:02Z"),
      ln(3, "── artifact upload …", "2026-06-12T10:00:03Z"),
      ln(4, "done"),
    ];
    const secs = sectionsOf(buildLogBlocks([], logs, 0));
    // setup section is the singleton over [1], labelled from the marker;
    // a marker-less LEADING block only appears when the first marker is
    // an OPEN. Here the first marker closes [1] as "task".
    expect(secs[0]?.label).toBe("task");
  });

  it("emits a `job setup` section when leading lines precede an OPEN marker", () => {
    const logs = [
      ln(1, "selecting toolchain"),
      ln(2, "── artifact upload …", "2026-06-12T10:00:02Z"),
      ln(3, "uploaded", "2026-06-12T10:00:03Z"),
      ln(4, "── artifact upload done in 1s", "2026-06-12T10:00:04Z"),
    ];
    const secs = sectionsOf(buildLogBlocks([], logs, 0));
    expect(secs[0]?.label).toBe("job setup");
    expect(secs[0]?.status).toBe("plain");
    expect(secs[0]?.lines.map((l) => l.seq)).toEqual([1]);
    expect(secs[1]?.label).toBe("artifact upload");
  });

  it("marks a trailing OPEN with no close as a running section, null duration", () => {
    const logs = [ln(1, "── cache store (1 entry) …"), ln(2, "storing")];
    const sec = sectionsOf(buildLogBlocks([], logs, 0))[0];
    expect(sec?.status).toBe("running");
    expect(sec?.durationSec).toBeNull();
    expect(sec?.label).toBe("cache store (1 entry)");
    expect(sec?.lines.map((l) => l.seq)).toEqual([2]);
  });

  it("never lets a section cross the omitted barrier", () => {
    const head = [ln(1, "── build …"), ln(2, "compiling")];
    const logs = [ln(50, "linking"), ln(51, "── build done in 90s", "2026-06-12T10:01:30Z")];
    const blocks = buildLogBlocks(head, logs, 47);
    // head's `build …` stays running (its close is past the gap);
    // an omitted divider sits between; logs starts a fresh segment.
    const kinds = blocks.map((b) => b.kind);
    expect(kinds).toContain("omitted");
    const omittedIdx = kinds.indexOf("omitted");
    // every head section is before the divider, every logs section after
    const headSec = sectionsOf(buildLogBlocks(head, [], 0))[0];
    expect(headSec?.status).toBe("running");
    expect(blocks[omittedIdx]).toEqual({ kind: "omitted", count: 47 });
  });

  it("parses hour-scale durations (Go's `1h2m3s`) on singleton summaries", () => {
    const logs = [
      ln(1, "compiling everything"),
      ln(2, "── build completed in 1h2m3s", "2026-06-12T11:02:03Z"),
    ];
    const sec = sectionsOf(buildLogBlocks([], logs, 0))[0];
    expect(sec?.status).toBe("success");
    expect(sec?.durationSec).toBe(3723);
  });

  it("defaultCollapsedIds collapses only success sections", () => {
    const logs = [
      ln(1, "── a …"),
      ln(2, "x"),
      ln(3, "── a done in 1s", "2026-06-12T10:00:03Z"),
      ln(4, "── b …", "2026-06-12T10:00:04Z"),
      ln(5, "y"),
      ln(6, "── b failed after 1s (exit 2)", "2026-06-12T10:00:05Z"),
      ln(7, "── c …", "2026-06-12T10:00:06Z"),
      ln(8, "tail"),
    ];
    const blocks = buildLogBlocks([], logs, 0);
    const secs = sectionsOf(blocks);
    const collapsed = defaultCollapsedIds(blocks);
    const success = secs.find((s) => s.status === "success");
    const failed = secs.find((s) => s.status === "failed");
    const running = secs.find((s) => s.status === "running");
    expect(success && collapsed.has(success.id)).toBe(true);
    expect(failed && collapsed.has(failed.id)).toBe(false);
    expect(running && collapsed.has(running.id)).toBe(false);
  });

  it("sectionIdForSeq maps an output line to its section, null for absorbed markers", () => {
    const logs = [
      ln(1, "── a …"),
      ln(2, "body"),
      ln(3, "── a done in 1s", "2026-06-12T10:00:03Z"),
    ];
    const blocks = buildLogBlocks([], logs, 0);
    const sec = sectionsOf(blocks)[0];
    expect(sectionIdForSeq(blocks, 2)).toBe(sec?.id);
    // marker seqs are absorbed into the header → not addressable
    expect(sectionIdForSeq(blocks, 1)).toBeNull();
    expect(sectionIdForSeq(blocks, 3)).toBeNull();
  });
});
