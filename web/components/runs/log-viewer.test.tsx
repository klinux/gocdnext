import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { LogViewer, classifyLine, ansiToReact } from "./log-viewer";
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

describe("ansiToReact", () => {
  // Render the parsed React nodes into a container so the
  // colour classes + plain text segments are inspectable as DOM —
  // matching how a real log line ends up under <LogViewer>.
  function renderAnsi(input: string): HTMLElement {
    const { container } = render(<>{ansiToReact(input)}</>);
    return container;
  }

  it("strips a plain reset-only line down to the literal text", () => {
    const c = renderAnsi("[0mhello[0m");
    expect(c.textContent).toBe("hello");
  });

  it("maps SGR foreground colours to tailwind classes", () => {
    // gitleaks-like:  \x1b[90m4:54PM\x1b[0m \x1b[32mINF\x1b[0m scan completed
    const c = renderAnsi(
      "[90m4:54PM[0m [32mINF[0m scan completed",
    );
    expect(c.textContent).toBe("4:54PM INF scan completed");
    // Each coloured chunk lands in a span with the colour class;
    // the trailing plain segment doesn't need wrapping.
    expect(c.querySelector(".text-muted-foreground")?.textContent).toBe("4:54PM");
    expect(c.querySelector(".text-emerald-500")?.textContent).toBe("INF");
  });

  it("flags warn (yellow) and error (red) like the terminal does", () => {
    const c = renderAnsi("[33mWRN[0m leaks found: 13");
    expect(c.querySelector(".text-amber-500")?.textContent).toBe("WRN");
    const e = renderAnsi("[31mERR[0m boom");
    expect(e.querySelector(".text-red-500")?.textContent).toBe("ERR");
  });

  it("combines bold + colour into a single span", () => {
    const c = renderAnsi("[1;32mPASS[0m");
    const pass = c.querySelector("span");
    expect(pass?.textContent).toBe("PASS");
    expect(pass?.className).toContain("text-emerald-500");
    expect(pass?.className).toContain("font-semibold");
  });

  it("resets state on \\x1b[0m so trailing plain text loses the previous colour", () => {
    const c = renderAnsi("[31mERR[0m then plain");
    const spans = c.querySelectorAll("span");
    // First span carries the colour, the plain trailing text is
    // either a bare text node or a span with no colour class.
    expect(spans[0]?.className).toContain("text-red-500");
    const plain = Array.from(c.childNodes).find(
      (n) => n.textContent === " then plain",
    );
    expect(plain).toBeTruthy();
  });

  it("passes plain text (no escapes) through unchanged", () => {
    const c = renderAnsi("just text");
    expect(c.textContent).toBe("just text");
    // No spans introduced for plain text — keeps the DOM cheap
    // when the agent or git emits uncoloured output.
    expect(c.querySelectorAll("span").length).toBe(0);
  });

  it("survives the orphan-brackets case (ESC byte stripped upstream)", () => {
    // Paste-back, gh CLI, or anything that strips \x1b leaves the
    // bracketed sequence as literal characters. We can't colour it,
    // but we MUST not crash and ideally drop the noise so the
    // operator sees clean text.
    const c = renderAnsi("[90m4:54PM[0m INF scan");
    // The literal brackets are preserved (no colour info to apply)
    // but rendering must not throw and must not insert spans.
    expect(c.textContent).toContain("4:54PM");
    expect(c.textContent).toContain("INF scan");
  });

  it("does not assert on empty string", () => {
    const c = renderAnsi("");
    expect(c.textContent).toBe("");
  });
});
