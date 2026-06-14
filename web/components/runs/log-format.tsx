import { Fragment, type ReactNode } from "react";
import { cn } from "@/lib/utils";

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
