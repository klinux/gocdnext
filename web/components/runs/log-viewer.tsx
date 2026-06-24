import { ChevronDown, ChevronRight } from "lucide-react";
import { cn } from "@/lib/utils";
import type { LogLine } from "@/types/api";
import { ansiToReact } from "@/components/runs/log-format";
import type { LogBlock, LogSection, SectionStatus } from "@/components/runs/log-sections";

// Re-exported for existing importers (and the viewer test) — the ANSI
// renderer moved to log-format.tsx to keep this file under budget.
export { ansiToReact };

type Props = {
  logs: LogLine[];
  // head, when provided, is rendered ABOVE the tail with a visual
  // divider between them showing `omitted` lines were trimmed.
  head?: LogLine[];
  omitted?: number;
  // jobStartedAt anchors the per-line elapsed time displayed on the
  // right (Woodpecker-style). When omitted, the timing column drops.
  jobStartedAt?: string;
  // matchSeqs marks lines as search hits; activeSeq is the focused hit.
  matchSeqs?: Set<number>;
  activeSeq?: number;
  // Folded mode (#48): when `blocks` is provided, render collapsible
  // phase sections instead of the flat head/omitted/logs split.
  // `collapsed` holds the folded section ids; `onToggleSection` toggles
  // one. The component stays presentational — fold STATE lives in
  // LogPane, which also force-opens sections for search / permalinks.
  blocks?: LogBlock[];
  collapsed?: Set<string>;
  onToggleSection?: (id: string) => void;
  className?: string;
};

export function LogViewer({
  logs,
  head,
  omitted,
  jobStartedAt,
  matchSeqs,
  activeSeq,
  blocks,
  collapsed,
  onToggleSection,
  className,
}: Props) {
  const hasHead = (head?.length ?? 0) > 0;
  const hasOmitted = (omitted ?? 0) > 0;
  const folded = blocks !== undefined;
  // Empty state holds in both modes: folded mode always receives a
  // `blocks` array, so a no-line job arrives here as `blocks=[]` — not
  // covered by the flat (logs/head) check.
  const isEmpty = folded ? blocks.length === 0 : logs.length === 0 && !hasHead;
  if (isEmpty) {
    return (
      <p className="px-3 py-2 text-xs text-muted-foreground">No log lines captured.</p>
    );
  }

  // Elapsed labels are computed across the FULL ordered line set so the
  // "show the second only when it changes" dedup stays continuous
  // across section boundaries (a collapsed section doesn't reset it).
  const orderedLines = folded
    ? (blocks ?? []).flatMap((b) => (b.kind === "section" ? b.section.lines : []))
    : [...(head ?? []), ...logs];
  const { show: showElapsed, labels } = computeElapsedLabels(orderedLines, jobStartedAt);

  const renderLine = (line: LogLine) => (
    <LogLineRow
      key={line.seq}
      line={line}
      elapsedLabel={labels.get(line.seq) ?? null}
      showElapsed={showElapsed}
      isMatch={matchSeqs?.has(line.seq) ?? false}
      isActive={activeSeq === line.seq}
    />
  );

  return (
    <pre
      className={cn(
        "max-h-96 overflow-auto bg-muted/40 px-3 py-2 font-mono text-xs leading-5",
        className,
      )}
    >
      {folded ? (
        (blocks ?? []).map((b, i) =>
          b.kind === "omitted" ? (
            <OmittedDivider key={`omitted-${i}`} count={b.count} />
          ) : (
            <Section
              key={b.section.id}
              section={b.section}
              collapsed={collapsed?.has(b.section.id) ?? false}
              onToggle={onToggleSection}
              renderLine={renderLine}
            />
          ),
        )
      ) : (
        <>
          {hasHead && head?.map(renderLine)}
          {hasHead && hasOmitted && <OmittedDivider count={omitted ?? 0} />}
          {logs.map(renderLine)}
        </>
      )}
    </pre>
  );
}

function OmittedDivider({ count }: { count: number }) {
  return (
    <div
      data-divider="omitted"
      className="my-1 grid grid-cols-1 border-t border-b border-dashed border-muted-foreground/30 bg-muted/20 px-2 py-1 text-center text-[10px] uppercase tracking-wide text-muted-foreground"
      aria-label={`${count} log lines omitted between head and tail`}
    >
      · · · {count.toLocaleString()} lines omitted · · ·
    </div>
  );
}

const statusDot: Record<SectionStatus, string> = {
  success: "bg-emerald-500",
  failed: "bg-red-500",
  running: "bg-amber-500 animate-pulse",
  plain: "bg-muted-foreground/40",
};

// Section renders one phase. A non-foldable section (the whole-log
// plain block when there are no markers) renders its lines flat — no
// header — so a marker-less log looks exactly like the pre-#48 view.
function Section({
  section,
  collapsed,
  onToggle,
  renderLine,
}: {
  section: LogSection;
  collapsed: boolean;
  onToggle?: (id: string) => void;
  renderLine: (line: LogLine) => React.ReactNode;
}) {
  if (!section.foldable) {
    return <>{section.lines.map(renderLine)}</>;
  }
  // Only an actually-running phase reads "running". A terminal or plain
  // section with no duration (e.g. "job setup", or a singleton whose
  // text we couldn't parse) just omits the label rather than lying.
  const dur =
    section.durationSec !== null
      ? formatElapsed(section.durationSec)
      : section.status === "running"
        ? "running"
        : "";
  return (
    <div data-section={section.id} data-status={section.status}>
      <button
        type="button"
        onClick={() => onToggle?.(section.id)}
        aria-expanded={!collapsed}
        className="flex w-full items-center gap-2 border-y border-border/40 bg-muted/30 px-1 py-0.5 text-left text-[11px] text-muted-foreground hover:bg-muted/60"
      >
        {collapsed ? (
          <ChevronRight className="size-3 shrink-0" />
        ) : (
          <ChevronDown className="size-3 shrink-0" />
        )}
        <span className={cn("size-1.5 shrink-0 rounded-full", statusDot[section.status])} />
        <span className="truncate font-semibold text-foreground/80">{section.label}</span>
        <span className="ml-auto shrink-0 tabular-nums">{dur}</span>
        {collapsed ? (
          <span className="shrink-0 tabular-nums">{section.lines.length} lines</span>
        ) : null}
      </button>
      {!collapsed && section.lines.map(renderLine)}
    </div>
  );
}

function LogLineRow({
  line,
  elapsedLabel,
  showElapsed,
  isMatch,
  isActive,
}: {
  line: LogLine;
  elapsedLabel: string | null;
  showElapsed: boolean;
  isMatch: boolean;
  isActive: boolean;
}) {
  const tone = classifyLine(line.text, line.stream);
  const cols = showElapsed
    ? "grid grid-cols-[3rem_1fr_3.5rem] gap-3"
    : "grid grid-cols-[3rem_1fr] gap-3";
  return (
    <div
      id={`L${line.seq}`}
      data-stream={line.stream}
      data-tone={tone}
      data-match={isMatch || undefined}
      className={cn(
        cols,
        toneClass[tone],
        isMatch && "bg-amber-500/10",
        isActive && "bg-amber-500/25",
      )}
    >
      {/* Line number doubles as the permalink anchor — click to put
          #L<seq> in the URL for sharing. */}
      <a
        href={`#L${line.seq}`}
        className="select-none text-right text-muted-foreground hover:text-foreground"
      >
        {line.seq}
      </a>
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
}

// computeElapsedLabels pre-renders the right-aligned elapsed column.
// The second is shown ONLY when it differs from the previously shown
// line, matching the Woodpecker timing column. Returns a per-seq map
// so the renderer is order-independent (sections render in document
// order; the map carries the dedup result).
function computeElapsedLabels(
  lines: LogLine[],
  jobStartedAt?: string,
): { show: boolean; labels: Map<number, string | null> } {
  const start = jobStartedAt ? Date.parse(jobStartedAt) : Number.NaN;
  const show = Number.isFinite(start);
  const labels = new Map<number, string | null>();
  if (!show) return { show, labels };
  let lastShown = -1;
  for (const line of lines) {
    const t = Date.parse(line.at);
    if (Number.isFinite(t)) {
      const sec = Math.max(0, Math.floor((t - start) / 1000));
      if (sec !== lastShown) {
        labels.set(line.seq, formatElapsed(sec));
        lastShown = sec;
        continue;
      }
    }
    labels.set(line.seq, null);
  }
  return { show, labels };
}

// formatElapsed renders cumulative seconds the way Woodpecker does:
// seconds, then minutes, then minutes + seconds for the long tail.
function formatElapsed(sec: number): string {
  if (sec < 60) return `${sec}s`;
  const m = Math.floor(sec / 60);
  const s = sec % 60;
  if (s === 0) return `${m}m`;
  return `${m}m${s}s`;
}

// LogTone classifies a line into a semantic colour bucket. Priority
// matters — the first matching rule wins so a specific signal beats
// the generic "stderr fallback".
type LogTone = "error" | "warn" | "success" | "command" | "muted" | "default";

export function classifyLine(text: string, stream: string): LogTone {
  const t = text.trim();
  if (t === "") return "default";

  // Command echoes: "$ <command>" lines. Bolded so the reader can skip
  // over them or anchor to them while scanning for output.
  if (t.startsWith("$ ")) return "command";

  // Unicode status glyphs used by test runners / lint tools.
  if (/^(✓|✔|PASS\b|SUCCESS\b|OK\b)/.test(t)) return "success";
  if (/^(✗|✘|❌|✕)/.test(t)) return "error";
  if (/^(⚠)/.test(t)) return "warn";

  // Go test structured output.
  if (/^---\s+(PASS|ok)\b/.test(t)) return "success";
  if (/^---\s+FAIL\b/.test(t)) return "error";
  if (/^(FAIL\s|FAIL$)/.test(t)) return "error";
  if (/^ok\s+\S/.test(t)) return "success";

  // Gradle build results — `console: plain` emits no colour, so the
  // success/failure semantics come from here (not ANSI). Anchored to
  // Gradle's exact lines on purpose: a broad \bFAILED\b would paint
  // ordinary prose red, and colouring every `> Task ...` line would be
  // visual noise — only the final result and failing tasks are flagged.
  if (/^BUILD SUCCESSFUL\b/.test(t)) return "success";
  if (/^BUILD FAILED\b/.test(t)) return "error";
  if (/^FAILURE: Build failed with an exception\.?$/.test(t)) return "error";
  if (/^> Task\b.*\bFAILED$/.test(t)) return "error";
  if (/^Deprecated Gradle features were used\b/.test(t)) return "warn";

  // Conventional log-level prefixes (case-sensitive to limit false
  // positives — "error" inside a normal sentence shouldn't paint red).
  if (/^(ERROR|FATAL|Error|Fatal|panic):/.test(t)) return "error";
  if (/^(WARN|WARNING|Warning|warning):/.test(t)) return "warn";
  if (/^(DEBUG|Debug|TRACE|Trace):/.test(t)) return "muted";

  // Strong error markers anywhere in the line.
  if (/\bpanic:\s/.test(t)) return "error";
  if (/\b(ERR!|FATAL!)\b/.test(t)) return "error";
  if (/\bsegmentation fault\b/i.test(t)) return "error";

  // stderr without a specific match stays red — better over-flag a
  // benign stderr line than under-flag a real error.
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
