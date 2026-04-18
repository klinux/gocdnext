import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { LogViewer } from "./log-viewer";
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
