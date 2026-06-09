import { Fragment, type ReactNode } from "react";
import { cn } from "@/lib/utils";
import type { LogLine } from "@/types/api";

type Props = {
  logs: LogLine[];
  // head, when provided, is rendered ABOVE the tail with a visual
  // divider between them showing `omitted` lines were trimmed.
  // Used by the run-detail page to render long jobs as
  // "first N + (X lines omitted) + last M" — the startup phase
  // (which `logs` alone hides for 23k-line builds) survives.
  head?: LogLine[];
  // omitted is the line count between head and tail. When > 0 the
  // divider is rendered with the count; when 0 (head+tail covers
  // everything, or no head requested) no divider appears.
  omitted?: number;
  // jobStartedAt anchors the per-line elapsed time displayed on the
  // right (Woodpecker-style "0s", "2s", "6s"). When omitted, the
  // grid drops the timing column gracefully — useful for log
  // fragments rendered outside a job context (test fixtures, the
  // archive viewer's per-file tail, etc).
  jobStartedAt?: string;
  className?: string;
};

// Static server-rendered tail — good enough for a completed run. Live tailing
// (SSE) is a later slice; keeping this component pure makes that drop-in easy:
// the client variant just replaces `logs` with a streamed state.
export function LogViewer({ logs, head, omitted, jobStartedAt, className }: Props) {
  const hasHead = (head?.length ?? 0) > 0;
  const hasOmitted = (omitted ?? 0) > 0;
  if (logs.length === 0 && !hasHead) {
    return (
      <p className="px-3 py-2 text-xs text-muted-foreground">No log lines captured.</p>
    );
  }
  const start = jobStartedAt ? Date.parse(jobStartedAt) : Number.NaN;
  const showElapsed = Number.isFinite(start);
  // Elapsed second is shown ONLY when it differs from the previous
  // rendered line — keeps consecutive same-second lines uncluttered
  // (matches the Woodpecker right-aligned timing column). Tracked
  // via a top-scope mutable; the map below runs synchronously in a
  // single render so the implicit ordering is stable.
  let lastShown = -1;
  const renderLine = (line: LogLine) => {
    const tone = classifyLine(line.text, line.stream);
    let elapsedLabel: string | null = null;
    if (showElapsed) {
      const t = Date.parse(line.at);
      if (Number.isFinite(t)) {
        const sec = Math.max(0, Math.floor((t - start) / 1000));
        if (sec !== lastShown) {
          elapsedLabel = formatElapsed(sec);
          lastShown = sec;
        }
      }
    }
    const cols = showElapsed
      ? "grid grid-cols-[3rem_1fr_3.5rem] gap-3"
      : "grid grid-cols-[3rem_1fr] gap-3";
    return (
      <div
        key={line.seq}
        data-stream={line.stream}
        data-tone={tone}
        className={cn(cols, toneClass[tone])}
      >
        <span className="select-none text-right text-muted-foreground">{line.seq}</span>
        <span className="whitespace-pre-wrap break-all">{ansiToReact(line.text)}</span>
        {showElapsed && (
          <span
            className="select-none text-right text-muted-foreground tabular-nums"
            aria-label={elapsedLabel ? `${elapsedLabel} elapsed` : undefined}
          >
            {elapsedLabel ?? ""}
          </span>
        )}
      </div>
    );
  };

  return (
    <pre
      className={cn(
        "max-h-96 overflow-auto bg-muted/40 px-3 py-2 font-mono text-xs leading-5",
        className,
      )}
    >
      {hasHead && head?.map(renderLine)}
      {hasHead && hasOmitted && (
        <div
          data-divider="omitted"
          className="my-1 grid grid-cols-1 border-t border-b border-dashed border-muted-foreground/30 bg-muted/20 px-2 py-1 text-center text-[10px] uppercase tracking-wide text-muted-foreground"
          aria-label={`${omitted} log lines omitted between head and tail`}
        >
          · · · {omitted?.toLocaleString()} lines omitted · · ·
        </div>
      )}
      {logs.map(renderLine)}
    </pre>
  );
}

// formatElapsed renders the cumulative seconds the way Woodpecker
// does: small numbers stay in seconds, then minutes, then minutes
// + seconds for the long tail. Keeps the right-aligned column
// narrow (max 6 chars) — anything beyond ~99m is degenerate enough
// that the grid widening is acceptable.
function formatElapsed(sec: number): string {
  if (sec < 60) return `${sec}s`;
  const m = Math.floor(sec / 60);
  const s = sec % 60;
  if (s === 0) return `${m}m`;
  return `${m}m${s}s`;
}

// LogTone classifies a line into a semantic colour bucket. Priority
// matters — the first matching rule wins so a specific signal
// ("--- FAIL:") beats the generic "stderr fallback". Kept to a
// small set: every extra category dilutes the scan-ability of the
// log tail, which is the whole reason we're tinting in the first
// place.
type LogTone =
  | "error"
  | "warn"
  | "success"
  | "command"
  | "muted"
  | "default";

export function classifyLine(text: string, stream: string): LogTone {
  const t = text.trim();
  if (t === "") return "default";

  // Command echoes: emitted by the agent as "$ <command>" lines.
  // Bolded so the reader can visually skip over them or anchor to
  // them while scanning for the actual command output underneath.
  if (t.startsWith("$ ")) return "command";

  // Unicode status glyphs used by test runners / lint tools — direct
  // signal, no ambiguity vs. natural language.
  if (/^(✓|✔|PASS\b|SUCCESS\b|OK\b)/.test(t)) return "success";
  if (/^(✗|✘|❌|✕)/.test(t)) return "error";
  if (/^(⚠)/.test(t)) return "warn";

  // Go test structured output — `--- PASS: TestFoo` / `--- FAIL:` /
  // standalone PASS/FAIL summary lines. Also `FAIL\tpkg` /
  // `ok  \tpkg` package-level results.
  if (/^---\s+(PASS|ok)\b/.test(t)) return "success";
  if (/^---\s+FAIL\b/.test(t)) return "error";
  if (/^(FAIL\s|FAIL$)/.test(t)) return "error";
  if (/^ok\s+\S/.test(t)) return "success";

  // Conventional log-level prefixes (case-sensitive to limit false
  // positives — "error" as an English word inside a normal sentence
  // shouldn't paint the whole line red).
  if (/^(ERROR|FATAL|Error|Fatal|panic):/.test(t)) return "error";
  if (/^(WARN|WARNING|Warning|warning):/.test(t)) return "warn";
  if (/^(DEBUG|Debug|TRACE|Trace):/.test(t)) return "muted";

  // Strong error markers anywhere in the line — panics, core dumps,
  // and npm/pnpm-style "ERR!" prefixes.
  if (/\bpanic:\s/.test(t)) return "error";
  if (/\b(ERR!|FATAL!)\b/.test(t)) return "error";
  if (/\bsegmentation fault\b/i.test(t)) return "error";

  // stderr without a specific match stays red — better over-flag a
  // benign stderr line than under-flag a real error. Downloads
  // like `go mod` and `git clone --progress` write to stderr by
  // convention, which is a known false positive accepted by every
  // CI UI I've seen.
  if (stream === "stderr") return "error";

  return "default";
}

const toneClass: Record<LogTone, string> = {
  error: "text-red-500",
  warn: "text-amber-500",
  success: "text-emerald-500",
  command: "font-semibold text-foreground",
  muted: "text-muted-foreground",
  default: "text-foreground",
};

// ANSI rendering: parse SGR escape sequences emitted by tools like
// gitleaks, trivy, go test (with `-v`) and turn them into React
// spans with tailwind colour classes. Keeps the same palette as
// classifyLine so colour semantics are consistent — "INF" green
// from a tool matches our own success tint.
//
// Scope is intentionally narrow: foreground colours (30-37, 90-97),
// bold (1), and reset (0). Backgrounds, italics, blink, underline
// and 256-colour / truecolour are ignored — they'd dilute the
// scan-ability of a build log without adding signal, and the
// noisier sequences (\x1b]…\x07 OSC, \x1b[?…h DECSET) are simply
// dropped.

const ANSI_SGR_RE = /\[([\d;]*)m/g;

type AnsiState = {
  fg: string | null;
  bold: boolean;
};

const fgClass: Record<number, string> = {
  // Standard foreground (30–37). 30 (black) and 37 (white) map to
  // semantic tokens so dark/light theme stays readable; the rest
  // hit a saturated 500-tone that survives both themes.
  30: "text-muted-foreground",
  31: "text-red-500",
  32: "text-emerald-500",
  33: "text-amber-500",
  34: "text-blue-500",
  35: "text-fuchsia-500",
  36: "text-cyan-500",
  37: "text-foreground",
  // Bright foreground (90–97). 90 is "bright black" = gray, used
  // heavily by structured loggers (zerolog, logrus, trivy) for
  // timestamps — keep it muted, not actual black, so it reads as
  // secondary.
  90: "text-muted-foreground",
  91: "text-red-400",
  92: "text-emerald-400",
  93: "text-amber-400",
  94: "text-blue-400",
  95: "text-fuchsia-400",
  96: "text-cyan-400",
  97: "text-foreground",
};

function applyCodes(state: AnsiState, codes: number[]): AnsiState {
  // Empty `\x1b[m` is shorthand for `\x1b[0m` — reset everything.
  if (codes.length === 0) {
    return { fg: null, bold: false };
  }
  let { fg, bold } = state;
  for (const code of codes) {
    if (code === 0) {
      fg = null;
      bold = false;
    } else if (code === 1) {
      bold = true;
    } else if (code === 22) {
      // 22 = reset bold/dim. Some tools emit it instead of full 0
      // to clear just the weight.
      bold = false;
    } else if (code === 39) {
      // 39 = default foreground (reset just the colour, leave bold).
      fg = null;
    } else if (fgClass[code] !== undefined) {
      fg = fgClass[code] ?? null;
    }
    // Unknown / unsupported (backgrounds 40–47, 100–107, italics 3,
    // underline 4, truecolour 38;2;…) silently ignored. Keeping the
    // surrounding text uncoloured is better than a half-applied
    // style.
  }
  return { fg, bold };
}

function styleClass(state: AnsiState): string {
  return cn(state.fg, state.bold && "font-semibold");
}

export function ansiToReact(text: string): ReactNode {
  if (text === "") return null;
  // Fast path: no escape byte → return as-is (no React span wrap)
  // so the typical plain-text line stays as a single text node.
  if (!text.includes("")) return text;

  const out: ReactNode[] = [];
  let state: AnsiState = { fg: null, bold: false };
  let cursor = 0;
  // Stable key per emitted segment — index suffices because the
  // segments are produced in order from a deterministic parse;
  // no reordering happens after render.
  let key = 0;

  ANSI_SGR_RE.lastIndex = 0;
  let match: RegExpExecArray | null = ANSI_SGR_RE.exec(text);
  while (match !== null) {
    const start = match.index;
    if (start > cursor) {
      const chunk = text.slice(cursor, start);
      const cls = styleClass(state);
      if (cls === "") {
        out.push(<Fragment key={key++}>{chunk}</Fragment>);
      } else {
        out.push(
          <span key={key++} className={cls}>
            {chunk}
          </span>,
        );
      }
    }
    const codes = match[1] === "" ? [] : (match[1] ?? "")
      .split(";")
      .map((s) => Number.parseInt(s, 10))
      .filter((n) => !Number.isNaN(n));
    state = applyCodes(state, codes);
    cursor = start + match[0].length;
    match = ANSI_SGR_RE.exec(text);
  }
  if (cursor < text.length) {
    const chunk = text.slice(cursor);
    const cls = styleClass(state);
    if (cls === "") {
      out.push(<Fragment key={key++}>{chunk}</Fragment>);
    } else {
      out.push(
        <span key={key++} className={cls}>
          {chunk}
        </span>,
      );
    }
  }
  return out;
}
