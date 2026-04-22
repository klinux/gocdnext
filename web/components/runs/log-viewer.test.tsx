import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { LogViewer, classifyLine } from "./log-viewer";
import type { LogLine } from "@/types/api";

const sample: LogLine[] = [
  { seq: 1, stream: "stdout", at: "2026-04-17T12:00:00Z", text: "compile ok" },
  { seq: 2, stream: "stderr", at: "2026-04-17T12:00:01Z", text: "warning: x" },
  { seq: 3, stream: "stdout", at: "2026-04-17T12:00:02Z", text: "done" },
];

describe("LogViewer", () => {
  it("renders each line with its sequence number", () => {
    render(<LogViewer logs={sample} />);
    expect(screen.getByText("compile ok")).toBeTruthy();
    expect(screen.getByText("warning: x")).toBeTruthy();
    expect(screen.getByText("done")).toBeTruthy();
    // Sequence numbers are shown as a gutter, once per line.
    expect(screen.getAllByText(/^\d+$/)).toHaveLength(3);
  });

  it("flags stderr rows with data-stream for styling hooks", () => {
    const { container } = render(<LogViewer logs={sample} />);
    const rows = container.querySelectorAll("[data-stream]");
    expect(rows.length).toBe(3);
    const stderrRows = container.querySelectorAll('[data-stream="stderr"]');
    expect(stderrRows.length).toBe(1);
  });

  it("shows an empty-state message when there are no logs", () => {
    render(<LogViewer logs={[]} />);
    expect(screen.getByText(/no log lines/i)).toBeTruthy();
  });
});

describe("classifyLine", () => {
  it("bolds command echoes", () => {
    expect(classifyLine("$ git clone foo", "stdout")).toBe("command");
  });

  it("flags go test PASS / FAIL markers", () => {
    expect(classifyLine("--- PASS: TestFoo (0.01s)", "stdout")).toBe("success");
    expect(classifyLine("--- FAIL: TestBar (0.01s)", "stdout")).toBe("error");
    expect(classifyLine("ok  	mypkg	0.02s", "stdout")).toBe("success");
    expect(classifyLine("FAIL	mypkg	0.02s", "stdout")).toBe("error");
  });

  it("catches conventional log-level prefixes case-sensitively", () => {
    expect(classifyLine("ERROR: database down", "stdout")).toBe("error");
    expect(classifyLine("WARN: retrying", "stdout")).toBe("warn");
    expect(classifyLine("DEBUG: token set", "stdout")).toBe("muted");
  });

  it("does not over-flag plain English containing the word 'error'", () => {
    // Regression guard — the heuristic used to paint this red.
    expect(classifyLine("returned an error code from the parser", "stdout")).toBe("default");
  });

  it("flags panic stack headers anywhere on the line", () => {
    expect(classifyLine("goroutine 1 panic: nil deref", "stdout")).toBe("error");
  });

  it("falls back to stderr=error when no specific pattern matches", () => {
    expect(classifyLine("some prose", "stderr")).toBe("error");
    expect(classifyLine("some prose", "stdout")).toBe("default");
  });

  it("recognises unicode status glyphs as a leading token", () => {
    expect(classifyLine("✓ all checks passed", "stdout")).toBe("success");
    expect(classifyLine("✗ deploy aborted", "stdout")).toBe("error");
    expect(classifyLine("⚠ retention backlog growing", "stdout")).toBe("warn");
  });
});
