import { cn } from "@/lib/utils";
import type { LogLine } from "@/types/api";

type Props = {
  logs: LogLine[];
  className?: string;
};

// Static server-rendered tail — good enough for a completed run. Live tailing
// (SSE) is a later slice; keeping this component pure makes that drop-in easy:
// the client variant just replaces `logs` with a streamed state.
export function LogViewer({ logs, className }: Props) {
  if (logs.length === 0) {
    return (
      <p className="px-3 py-2 text-xs text-muted-foreground">No log lines captured.</p>
    );
  }
  return (
    <pre
      className={cn(
        "max-h-96 overflow-auto bg-muted/40 px-3 py-2 font-mono text-xs leading-5",
        className,
      )}
    >
      {logs.map((line) => {
        const tone = classifyLine(line.text, line.stream);
        return (
          <div
            key={line.seq}
            data-stream={line.stream}
            data-tone={tone}
            className={cn("grid grid-cols-[3rem_1fr] gap-3", toneClass[tone])}
          >
            <span className="select-none text-right text-muted-foreground">
              {line.seq}
            </span>
            <span className="whitespace-pre-wrap break-all">{line.text}</span>
          </div>
        );
      })}
    </pre>
  );
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
