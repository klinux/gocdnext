"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  ArrowDownToLine,
  ArrowUpToLine,
  Check,
  ChevronDown,
  ChevronUp,
  Copy,
  Download,
  Pause,
  Play,
  Search,
} from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { LogViewer } from "@/components/runs/log-viewer";
import {
  buildLogBlocks,
  defaultCollapsedIds,
  sectionIdForSeq,
} from "@/components/runs/log-sections";
import { cn } from "@/lib/utils";
import type { LogLine } from "@/types/api";

type Props = {
  logs: LogLine[];
  head?: LogLine[];
  omitted?: number;
  jobStartedAt?: string;
  // running drives the follow affordance: a finished job has
  // nothing to follow, so the pill hides.
  running?: boolean;
  // downloadHref, when set, renders the Download button pointing at
  // the full-log export endpoint (the pane may only hold head+tail).
  downloadHref?: string;
  className?: string;
};

// LogPane is the interactive shell around LogViewer (#37, #48): smart
// follow, in-log search with n/N navigation, copy, download, #L<seq>
// permalinks, AND phase folding on the agent's `── ` markers. The
// viewer stays presentational; all fold STATE + coordination lives
// here so it can be reconciled with follow / search / permalinks.
export function LogPane({
  logs,
  head,
  omitted,
  jobStartedAt,
  running = false,
  downloadHref,
  className,
}: Props) {
  const scrollRef = useRef<HTMLDivElement>(null);
  const [follow, setFollow] = useState(running);
  // queued → running transition turns follow ON once.
  const wasRunning = useRef(running);
  useEffect(() => {
    if (running && !wasRunning.current) setFollow(true);
    wasRunning.current = running;
  }, [running]);
  const [query, setQuery] = useState("");
  const [matchIdx, setMatchIdx] = useState(0);
  const [copied, setCopied] = useState(false);

  // Phase blocks (#48). Recomputed as the log streams; ids are derived
  // from real line seqs so fold state survives appends.
  const blocks = useMemo(
    () => buildLogBlocks(head ?? [], logs, omitted ?? 0),
    [head, logs, omitted],
  );
  // Search scans only the RENDERED output lines — the `── ` markers are
  // absorbed into headers, so including them would inflate the count
  // and let n/N land on a line that isn't in the DOM.
  const renderedLines = useMemo(
    () => blocks.flatMap((b) => (b.kind === "section" ? b.section.lines : [])),
    [blocks],
  );
  const lastSectionId = useMemo(() => {
    for (let i = blocks.length - 1; i >= 0; i--) {
      const b = blocks[i];
      if (b?.kind === "section" && b.section.foldable) return b.section.id;
    }
    return null;
  }, [blocks]);

  // Base fold state (user intent + per-section default). A section is
  // defaulted ONCE, the first time it reaches a terminal status, so a
  // manual toggle afterwards wins (decision 3). Running sections are
  // never auto-collapsed.
  const [collapsed, setCollapsed] = useState<Set<string>>(() => defaultCollapsedIds(blocks));
  const defaulted = useRef<Set<string>>(new Set());
  useEffect(() => {
    setCollapsed((prev) => {
      let next = prev;
      for (const b of blocks) {
        if (b.kind !== "section") continue;
        const s = b.section;
        const terminal = s.status === "success" || s.status === "failed";
        if (terminal && !defaulted.current.has(s.id)) {
          defaulted.current.add(s.id);
          // Auto-collapse a completed success phase — but NOT if the
          // operator paused follow on a still-running job and may be
          // reading it (decision #3). A finished job (!running) always
          // tidies up; an actively-followed run folds as phases land.
          if (s.status === "success" && s.foldable && (!running || follow)) {
            if (next === prev) next = new Set(prev);
            next.add(s.id);
          }
        }
      }
      return next;
    });
  }, [blocks, running, follow]);

  // Permalink pins: sections force-opened to reveal a #L<seq> target.
  const [pinnedOpen, setPinnedOpen] = useState<Set<string>>(new Set());

  const matches = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (q === "") return [];
    return renderedLines.filter((l) => l.text.toLowerCase().includes(q)).map((l) => l.seq);
  }, [renderedLines, query]);
  const activeSeq =
    matches.length > 0 ? matches[Math.min(matchIdx, matches.length - 1)] : undefined;

  // Sections holding any match stay open while searching (decision 2).
  const matchSectionIds = useMemo(() => {
    const ids = new Set<string>();
    for (const seq of matches) {
      const sid = sectionIdForSeq(blocks, seq);
      if (sid) ids.add(sid);
    }
    return ids;
  }, [matches, blocks]);

  // Render state = base collapsed MINUS the always-open overrides:
  // the followed current section, sections with a search match, and
  // permalink-pinned sections. Overrides don't mutate `collapsed`, so
  // clearing the search / pin restores the user's folds.
  const effectiveCollapsed = useMemo(() => {
    let e = collapsed;
    const open = (id: string | null) => {
      if (id && e.has(id)) {
        if (e === collapsed) e = new Set(collapsed);
        e.delete(id);
      }
    };
    if (follow && running) open(lastSectionId);
    for (const id of matchSectionIds) open(id);
    for (const id of pinnedOpen) open(id);
    return e;
  }, [collapsed, follow, running, lastSectionId, matchSectionIds, pinnedOpen]);

  const onToggleSection = useCallback(
    (id: string) => {
      // Collapsing the followed current section is an intent to stop
      // following (decision 4).
      if (follow && running && id === lastSectionId) setFollow(false);
      setPinnedOpen((prev) => {
        if (!prev.has(id)) return prev;
        const n = new Set(prev);
        n.delete(id);
        return n;
      });
      setCollapsed((prev) => {
        const n = new Set(prev);
        if (n.has(id)) n.delete(id);
        else n.add(id);
        return n;
      });
    },
    [follow, running, lastSectionId],
  );

  const scrollToBottom = useCallback(() => {
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, []);
  const scrollToTop = useCallback(() => {
    const el = scrollRef.current;
    if (el) el.scrollTop = 0;
  }, []);

  useEffect(() => {
    if (follow && running) scrollToBottom();
  }, [logs.length, follow, running, scrollToBottom]);

  const onScroll = useCallback(() => {
    const el = scrollRef.current;
    if (!el || !running) return;
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
    setFollow(atBottom);
  }, [running]);

  // Permalink: honor #L<seq> on mount — force-open its section, then
  // scroll once the expand has rendered.
  useEffect(() => {
    const m = /^#L(\d+)$/.exec(window.location.hash);
    if (!m) return;
    const seq = Number(m[1]);
    const sid = sectionIdForSeq(blocks, seq);
    if (sid) setPinnedOpen(new Set([sid]));
    const raf = requestAnimationFrame(() => {
      const target = document.getElementById(`L${seq}`);
      if (target) target.scrollIntoView({ block: "center" });
    });
    return () => cancelAnimationFrame(raf);
    // One-shot on mount by design.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Scroll the active search match into view. Its section is already
  // force-open via matchSectionIds, so the line is in the DOM.
  useEffect(() => {
    if (activeSeq === undefined) return;
    const target = document.getElementById(`L${activeSeq}`);
    if (target) target.scrollIntoView({ block: "center" });
  }, [activeSeq]);

  const gotoMatch = useCallback(
    (dir: 1 | -1) => {
      if (matches.length === 0) return;
      setMatchIdx((i) => (i + dir + matches.length) % matches.length);
    },
    [matches.length],
  );

  const onCopy = useCallback(async () => {
    const parts: string[] = [];
    for (const l of head ?? []) parts.push(l.text);
    if ((omitted ?? 0) > 0) parts.push(`··· ${omitted} lines omitted ···`);
    for (const l of logs) parts.push(l.text);
    try {
      await navigator.clipboard.writeText(parts.join("\n"));
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      // Clipboard denied (permissions) — nothing useful to do.
    }
  }, [head, omitted, logs]);

  return (
    <div className={cn("flex max-h-96 flex-col", className)}>
      <div className="flex flex-wrap items-center gap-1 border-b border-border bg-muted/30 px-2 py-1">
        <div className="relative">
          <Search className="pointer-events-none absolute left-2 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setMatchIdx(0);
            }}
            onKeyDown={(e) => {
              if (e.key === "Enter") gotoMatch(e.shiftKey ? -1 : 1);
            }}
            placeholder="Search log…"
            aria-label="Search log"
            className="h-7 w-44 pl-7 text-xs"
          />
        </div>
        {query.trim() !== "" ? (
          <span className="text-xs tabular-nums text-muted-foreground">
            {matches.length === 0
              ? "0 matches"
              : `${Math.min(matchIdx + 1, matches.length)}/${matches.length}`}
          </span>
        ) : null}
        {matches.length > 0 ? (
          <>
            <Button variant="ghost" size="icon" className="size-7" aria-label="Previous match" onClick={() => gotoMatch(-1)}>
              <ChevronUp className="size-3.5" />
            </Button>
            <Button variant="ghost" size="icon" className="size-7" aria-label="Next match" onClick={() => gotoMatch(1)}>
              <ChevronDown className="size-3.5" />
            </Button>
          </>
        ) : null}

        <div className="ml-auto flex items-center gap-1">
          {running ? (
            <Button
              variant={follow ? "secondary" : "ghost"}
              size="sm"
              className="h-7 gap-1 px-2 text-xs"
              aria-label={follow ? "Following — click to pause" : "Follow log"}
              onClick={() => {
                const next = !follow;
                setFollow(next);
                if (next) scrollToBottom();
              }}
            >
              {follow ? <Pause className="size-3.5" /> : <Play className="size-3.5" />}
              {follow ? "Following" : "Follow"}
            </Button>
          ) : null}
          <Button variant="ghost" size="icon" className="size-7" aria-label="Jump to top" onClick={scrollToTop}>
            <ArrowUpToLine className="size-3.5" />
          </Button>
          <Button variant="ghost" size="icon" className="size-7" aria-label="Jump to bottom" onClick={scrollToBottom}>
            <ArrowDownToLine className="size-3.5" />
          </Button>
          <Button variant="ghost" size="icon" className="size-7" aria-label="Copy log" onClick={onCopy}>
            {copied ? <Check className="size-3.5 text-emerald-500" /> : <Copy className="size-3.5" />}
          </Button>
          {downloadHref ? (
            <Button
              variant="ghost"
              size="icon"
              className="size-7"
              aria-label="Download full log"
              nativeButton={false}
              render={<a href={downloadHref} download />}
            >
              <Download className="size-3.5" />
            </Button>
          ) : null}
        </div>
      </div>
      <div ref={scrollRef} onScroll={onScroll} className="min-h-0 flex-1 overflow-auto">
        <LogViewer
          logs={logs}
          head={head}
          omitted={omitted}
          jobStartedAt={jobStartedAt}
          matchSeqs={matches.length > 0 ? new Set(matches) : undefined}
          activeSeq={activeSeq}
          blocks={blocks}
          collapsed={effectiveCollapsed}
          onToggleSection={onToggleSection}
          className="max-h-none overflow-visible"
        />
      </div>
    </div>
  );
}
