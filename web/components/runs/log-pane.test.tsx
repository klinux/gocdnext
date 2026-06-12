import { fireEvent, render, screen } from "@testing-library/react";
import { beforeAll, describe, expect, it, vi } from "vitest";

import { LogPane } from "./log-pane.client";
import type { LogLine } from "@/types/api";

beforeAll(() => {
  // jsdom has no scrollIntoView; LogPane uses it for match/permalink
  // navigation.
  Element.prototype.scrollIntoView = vi.fn();
});

const lines: LogLine[] = [
  { seq: 1, stream: "stdout", at: "2026-06-12T10:00:00Z", text: "$ go test ./..." },
  { seq: 2, stream: "stdout", at: "2026-06-12T10:00:05Z", text: "--- FAIL: TestCart" },
  { seq: 3, stream: "stderr", at: "2026-06-12T10:00:06Z", text: "Error: assertion failed" },
  { seq: 4, stream: "stdout", at: "2026-06-12T10:00:07Z", text: "FAIL pkg/cart 0.3s" },
];

describe("LogPane", () => {
  it("searches with match count and n/N navigation", () => {
    render(<LogPane logs={lines} />);
    const input = screen.getByLabelText("Search log");
    fireEvent.change(input, { target: { value: "fail" } });
    // "FAIL"/"failed" appear on seqs 2, 3, 4 (case-insensitive).
    expect(screen.getByText("1/3")).toBeTruthy();
    fireEvent.keyDown(input, { key: "Enter" });
    expect(screen.getByText("2/3")).toBeTruthy();
    fireEvent.keyDown(input, { key: "Enter", shiftKey: true });
    expect(screen.getByText("1/3")).toBeTruthy();
    // Line-level highlight markers present.
    expect(document.querySelectorAll("[data-match]").length).toBe(3);
  });

  it("renders zero-match state without crashing", () => {
    render(<LogPane logs={lines} />);
    fireEvent.change(screen.getByLabelText("Search log"), {
      target: { value: "nonexistent-token" },
    });
    expect(screen.getByText("0 matches")).toBeTruthy();
  });

  it("shows the follow pill only while running", () => {
    const { rerender } = render(<LogPane logs={lines} running />);
    expect(screen.getByText("Following")).toBeTruthy();
    rerender(<LogPane logs={lines} running={false} />);
    expect(screen.queryByText("Following")).toBeNull();
    expect(screen.queryByText("Follow")).toBeNull();
  });

  it("copies head + omission note + tail", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });
    render(
      <LogPane
        logs={[lines[3]!]}
        head={[lines[0]!]}
        omitted={42}
      />,
    );
    fireEvent.click(screen.getByLabelText("Copy log"));
    await vi.waitFor(() => expect(writeText).toHaveBeenCalled());
    const copied = writeText.mock.calls[0]?.[0] as string;
    expect(copied).toContain("$ go test ./...");
    expect(copied).toContain("42 lines omitted");
    expect(copied).toContain("FAIL pkg/cart 0.3s");
  });

  it("renders the download button only when an href is given", () => {
    const { rerender } = render(
      <LogPane logs={lines} downloadHref="/api/v1/runs/r/jobs/j/log.txt" />,
    );
    expect(screen.getByLabelText("Download full log")).toBeTruthy();
    rerender(<LogPane logs={lines} />);
    expect(screen.queryByLabelText("Download full log")).toBeNull();
  });

  it("line numbers are permalink anchors", () => {
    render(<LogPane logs={lines} />);
    const anchor = screen.getByText("2").closest("a");
    expect(anchor?.getAttribute("href")).toBe("#L2");
    expect(document.getElementById("L2")).toBeTruthy();
  });
  it("starts following when the job transitions queued → running", () => {
    const { rerender } = render(<LogPane logs={lines} running={false} />);
    expect(screen.queryByText("Following")).toBeNull();
    rerender(<LogPane logs={lines} running />);
    // The transition flips follow ON even though mount sampled false.
    expect(screen.getByText("Following")).toBeTruthy();
  });
});
