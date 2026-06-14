import type { LogLine } from "@/types/api";

// Phase folding model (#48). The agent emits `── ` boundary markers
// (phase.go): open markers end with " …", close markers carry a status
// verb — "done in" / "completed in" (success) or "failed after"
// (failed). A timedPhase emits an open/close PAIR bracketing its body;
// task/tasks summaries are SINGLETON closes that summarise the lines
// before them. Markers never nest (isolated mode wraps one post-job
// pair; shared mode emits sibling pairs). The parser is deliberately
// flat and substring-driven — no ambitious grammar.

export type SectionStatus = "running" | "success" | "failed" | "plain";

export type LogSection = {
  // Stable id derived from a real line seq, so React keys and the
  // collapsed-set survive streaming appends.
  id: string;
  label: string;
  status: SectionStatus;
  // Output lines only — the `── ` markers are absorbed into the header,
  // so a #L<seq> to a marker won't resolve to a section (acceptable:
  // markers are decorative boundaries, not permalink targets).
  lines: LogLine[];
  durationSec: number | null;
  // false for the whole-log plain block when there are no markers — it
  // renders flat (no header), preserving the pre-#48 view.
  foldable: boolean;
};

export type LogBlock =
  | { kind: "section"; section: LogSection }
  | { kind: "omitted"; count: number };

const MARKER_PREFIX = "── ";

function isMarker(line: LogLine): boolean {
  return line.text.startsWith(MARKER_PREFIX);
}

type Marker =
  | { kind: "open"; label: string }
  | { kind: "close"; label: string; status: "success" | "failed"; durText: string };

function parseMarker(text: string): Marker {
  const rest = text.slice(MARKER_PREFIX.length);
  // Close (success): "<label> done in <dur>" / "<label> completed in <dur>"
  const ok = /^(.*) (?:done|completed) in (.+)$/.exec(rest);
  if (ok) return { kind: "close", label: ok[1] ?? "", status: "success", durText: ok[2] ?? "" };
  // Close (failed): "<label> failed after <dur><detail>" — keep the
  // trailing "(task 2, exit 1)" detail on the label so the header shows
  // WHY it failed.
  const bad = /^(.*?) failed after (\S+)(.*)$/.exec(rest);
  if (bad) {
    return {
      kind: "close",
      label: `${bad[1] ?? ""}${bad[3] ?? ""}`.trim(),
      status: "failed",
      durText: bad[2] ?? "",
    };
  }
  // Open: trailing ellipsis (timedPhase emits `<label> …`). Strip it.
  const trimmed = rest.replace(/\s*…\s*$/, "").trimEnd();
  return { kind: "open", label: trimmed };
}

function atDiffSec(fromAt: string, toAt: string): number | null {
  const a = Date.parse(fromAt);
  const b = Date.parse(toAt);
  if (!Number.isFinite(a) || !Number.isFinite(b)) return null;
  return Math.max(0, Math.round((b - a) / 1000));
}

// parseDurText turns the agent's own "5s" / "1m30s" / "<1s" into
// seconds — the authoritative number for singleton summaries, which
// have no open marker to diff against (decision 6).
function parseDurText(s: string): number | null {
  const t = s.trim();
  if (t === "<1s") return 0;
  // Go's time.Duration.String() emits "1h0m0s" past the hour mark, so
  // an hours arm is mandatory — without it a >1h phase fails to parse
  // and the header falls back to a misleading label.
  const hm = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$/.exec(t);
  if (!hm || (hm[1] === undefined && hm[2] === undefined && hm[3] === undefined)) {
    return null;
  }
  return (Number(hm[1] ?? 0) * 3600) + (Number(hm[2] ?? 0) * 60) + Number(hm[3] ?? 0);
}

function idFor(lines: LogLine[], fallbackSeq: number): string {
  return `s${lines[0]?.seq ?? fallbackSeq}`;
}

function sectionsOfSegment(lines: LogLine[]): LogSection[] {
  const out: LogSection[] = [];
  let pending: LogLine[] = []; // non-marker lines awaiting a section
  let open: { marker: LogLine; label: string } | null = null;
  let openBody: LogLine[] = [];

  const flushPendingPlain = () => {
    if (pending.length === 0) return;
    // Leading lines before the first section → "job setup"; loose
    // inter-phase output → unlabelled (renders flat, no header).
    const label = out.length === 0 ? "job setup" : "";
    out.push({
      id: idFor(pending, pending[0]?.seq ?? 0),
      label,
      status: "plain",
      lines: pending,
      durationSec: null,
      foldable: label !== "",
    });
    pending = [];
  };

  for (const line of lines) {
    if (!isMarker(line)) {
      if (open) openBody.push(line);
      else pending.push(line);
      continue;
    }
    const m = parseMarker(line.text);
    if (m.kind === "open") {
      flushPendingPlain();
      if (open) {
        // Defensive: a second open with no intervening close (shouldn't
        // happen — markers are flat). Close the prior as running.
        out.push({
          id: idFor(openBody, open.marker.seq),
          label: open.label,
          status: "running",
          lines: openBody,
          durationSec: null,
          foldable: true,
        });
      }
      open = { marker: line, label: m.label };
      openBody = [];
    } else {
      if (open) {
        out.push({
          id: idFor(openBody, open.marker.seq),
          label: m.label,
          status: m.status,
          lines: openBody,
          durationSec: atDiffSec(open.marker.at, line.at),
          foldable: true,
        });
        open = null;
        openBody = [];
      } else {
        // Singleton trailing summary over the pending lines.
        out.push({
          id: idFor(pending, line.seq),
          label: m.label,
          status: m.status,
          lines: pending,
          durationSec: parseDurText(m.durText),
          foldable: true,
        });
        pending = [];
      }
    }
  }

  if (open) {
    out.push({
      id: idFor(openBody, open.marker.seq),
      label: open.label,
      status: "running",
      lines: openBody,
      durationSec: null,
      foldable: true,
    });
  } else if (pending.length > 0) {
    // No markers at all → one flat plain block (label "", not foldable,
    // preserves the pre-#48 view). Trailing loose output after sections
    // → "current output".
    const label = out.length === 0 ? "" : "current output";
    out.push({
      id: idFor(pending, pending[0]?.seq ?? 0),
      label,
      status: "plain",
      lines: pending,
      durationSec: null,
      foldable: label !== "",
    });
  }

  return out;
}

// buildLogBlocks groups head + tail into ordered render blocks. The
// omitted divider is a HARD barrier (decision 7): head and tail are
// sectioned independently so a section never spans the gap.
export function buildLogBlocks(head: LogLine[], logs: LogLine[], omitted: number): LogBlock[] {
  const blocks: LogBlock[] = [];
  if (head.length > 0) {
    for (const section of sectionsOfSegment(head)) blocks.push({ kind: "section", section });
    if (omitted > 0) blocks.push({ kind: "omitted", count: omitted });
  }
  for (const section of sectionsOfSegment(logs)) blocks.push({ kind: "section", section });
  return blocks;
}

// defaultCollapsedIds collapses only successful phases (decision 2):
// running, failed and plain sections stay open. Search/permalink
// force-open is layered on top by the caller, not here.
export function defaultCollapsedIds(blocks: LogBlock[]): Set<string> {
  const ids = new Set<string>();
  for (const b of blocks) {
    if (b.kind === "section" && b.section.status === "success" && b.section.foldable) {
      ids.add(b.section.id);
    }
  }
  return ids;
}

// sectionIdForSeq maps an output line's seq to the id of the section
// containing it — used to force-open the section around a search match
// or #L<seq> permalink before scrolling. Returns null for absorbed
// marker seqs and for seqs outside any section.
export function sectionIdForSeq(blocks: LogBlock[], seq: number): string | null {
  for (const b of blocks) {
    if (b.kind !== "section") continue;
    if (b.section.lines.some((l) => l.seq === seq)) return b.section.id;
  }
  return null;
}
