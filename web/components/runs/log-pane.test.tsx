import { fireEvent, render, screen } from "@testing-library/react";
import { beforeAll, describe, expect, it, vi } from "vitest";

import { LogPane } from "./log-pane.client";
import type { LogLine } from "@/types/api";

beforeAll(() => {
  // jsdom has no scrollIntoView; LogPane uses it for match/permalink
  // navigation.
  Element.prototype.scrollIntoView = vi.fn();
  // The permalink effect schedules its scroll in a rAF; run it inline
  // so jsdom doesn't choke and the (mocked) scroll fires deterministically.
  globalThis.requestAnimationFrame = ((cb: FrameRequestCallback) => {
    cb(0);
    return 0;
  }) as typeof globalThis.requestAnimationFrame;
  globalThis.cancelAnimationFrame = (() => {}) as typeof globalThis.cancelAnimationFrame;
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

// Phase folding (#48). The agent emits `── ` boundary markers; LogPane
// groups the log into collapsible sections.
const phased: LogLine[] = [
  { seq: 1, stream: "stdout", at: "2026-06-12T10:00:00Z", text: "── deps …" },
  { seq: 2, stream: "stdout", at: "2026-06-12T10:00:01Z", text: "installed lodash" },
  { seq: 3, stream: "stdout", at: "2026-06-12T10:00:03Z", text: "── deps done in 3s" },
  { seq: 4, stream: "stdout", at: "2026-06-12T10:00:04Z", text: "── tests …" },
  { seq: 5, stream: "stderr", at: "2026-06-12T10:00:05Z", text: "assertion failed: want 2" },
  { seq: 6, stream: "stdout", at: "2026-06-12T10:00:08Z", text: "── tests failed after 4s (exit 1)" },
  { seq: 7, stream: "stdout", at: "2026-06-12T10:00:09Z", text: "── deploy …" },
  { seq: 8, stream: "stdout", at: "2026-06-12T10:00:10Z", text: "shipping" },
];

describe("LogPane phase folding", () => {
  it("collapses successful phases by default, leaves failed + running open", () => {
    render(<LogPane logs={phased} />);
    // deps succeeded → body folded away.
    expect(screen.queryByText("installed lodash")).toBeNull();
    // failed + running phases stay open.
    expect(screen.getByText(/assertion failed/)).toBeTruthy();
    expect(screen.getByText("shipping")).toBeTruthy();
    // the collapsed phase still shows its header (label + duration).
    expect(screen.getByRole("button", { name: /deps/ })).toBeTruthy();
  });

  it("toggles a section open on header click; manual state wins (decision 3)", () => {
    render(<LogPane logs={phased} />);
    // open the collapsed deps phase
    fireEvent.click(screen.getByRole("button", { name: /deps/ }));
    expect(screen.getByText("installed lodash")).toBeTruthy();
    // collapse the default-open failed phase — manual close holds
    fireEvent.click(screen.getByRole("button", { name: /tests/ }));
    expect(screen.queryByText(/assertion failed/)).toBeNull();
  });

  it("expands the section containing a search match (decision 2/5)", () => {
    render(<LogPane logs={phased} />);
    expect(screen.queryByText("installed lodash")).toBeNull();
    fireEvent.change(screen.getByLabelText("Search log"), {
      target: { value: "installed" },
    });
    // the match lives in the collapsed deps phase → it force-opens.
    expect(screen.getByText("installed lodash")).toBeTruthy();
  });

  it("does not count `── ` markers absorbed into headers as search hits", () => {
    render(<LogPane logs={phased} />);
    // "deps" only appears in the marker `── deps …` (the header), never
    // in a rendered output line → no navigable matches.
    fireEvent.change(screen.getByLabelText("Search log"), {
      target: { value: "deps" },
    });
    expect(screen.getByText("0 matches")).toBeTruthy();
  });

  it("expands the section a #L<seq> permalink targets (decision 5)", () => {
    window.location.hash = "#L2"; // line 2 lives in the collapsed deps phase
    try {
      render(<LogPane logs={phased} />);
      expect(screen.getByText("installed lodash")).toBeTruthy();
    } finally {
      window.location.hash = "";
    }
  });

  it("shows the empty state with no log lines, even in folded mode", () => {
    render(<LogPane logs={[]} />);
    expect(screen.getByText("No log lines captured.")).toBeTruthy();
  });

  it("never labels a plain `job setup` section as running", () => {
    const withSetup: LogLine[] = [
      { seq: 1, stream: "stdout", at: "2026-06-12T10:00:00Z", text: "selecting toolchain" },
      { seq: 2, stream: "stdout", at: "2026-06-12T10:00:01Z", text: "── deps …" },
      { seq: 3, stream: "stdout", at: "2026-06-12T10:00:02Z", text: "installed" },
      { seq: 4, stream: "stdout", at: "2026-06-12T10:00:04Z", text: "── deps done in 2s" },
    ];
    render(<LogPane logs={withSetup} />);
    expect(screen.getByRole("button", { name: /job setup/ })).toBeTruthy();
    // a finished run has no running phase → the "running" label must
    // not leak onto the plain setup (or any) header.
    expect(screen.queryByText("running")).toBeNull();
  });

  it("keeps a completed success phase open while follow is paused on a running job (#3)", () => {
    const open: LogLine[] = [
      { seq: 1, stream: "stdout", at: "2026-06-12T10:00:00Z", text: "── deps …" },
      { seq: 2, stream: "stdout", at: "2026-06-12T10:00:01Z", text: "installed lodash" },
    ];
    const closed: LogLine[] = [
      ...open,
      { seq: 3, stream: "stdout", at: "2026-06-12T10:00:03Z", text: "── deps done in 3s" },
    ];
    const { rerender } = render(<LogPane logs={open} running />);
    // deps is still running → open; pause follow, then it completes.
    expect(screen.getByText("installed lodash")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: /Following/ }));
    rerender(<LogPane logs={closed} running />);
    // paused operator may be reading it → not yanked closed.
    expect(screen.getByText("installed lodash")).toBeTruthy();
  });

  it("keeps the omitted divider outside any collapsible section (decision 7)", () => {
    const head: LogLine[] = [
      { seq: 1, stream: "stdout", at: "2026-06-12T10:00:00Z", text: "── build …" },
      { seq: 2, stream: "stdout", at: "2026-06-12T10:00:01Z", text: "compiling" },
    ];
    const tail: LogLine[] = [
      { seq: 50, stream: "stdout", at: "2026-06-12T10:01:00Z", text: "linking" },
      { seq: 51, stream: "stdout", at: "2026-06-12T10:01:30Z", text: "── build done in 90s" },
    ];
    render(<LogPane head={head} logs={tail} omitted={47} />);
    const divider = screen.getByLabelText(/47 log lines omitted/);
    expect(divider).toBeTruthy();
    // not nested inside a [data-section] — it's a top-level barrier.
    expect(divider.closest("[data-section]")).toBeNull();
  });
});
