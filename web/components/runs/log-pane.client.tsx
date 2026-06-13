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

// LogPane is the interactive shell around LogViewer (#37): smart
// follow (tail -f that pauses when you scroll up), jump to top /
// bottom, in-log search with n/N navigation, copy, download, and
// #L<seq> permalinks. The viewer stays pure; everything stateful
// lives here.
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
  // queued → running transition turns follow ON once (review LOW:
  // useState(running) only samples mount time, so a card mounted
  // while queued never started following). A manual pause AFTER
  // the transition is respected — the effect only fires on the
  // false→true edge.
  const wasRunning = useRef(running);
  useEffect(() => {
    if (running && !wasRunning.current) setFollow(true);
    wasRunning.current = running;
  }, [running]);
  const [query, setQuery] = useState("");
  const [matchIdx, setMatchIdx] = useState(0);
  const [copied, setCopied] = useState(false);

  const allLines = useMemo(
    () => [...(head ?? []), ...logs],
    [head, logs],
  );

  // Line-level matches: substring, case-insensitive. Highlighting is
  // per LINE (not per substring) on purpose — the viewer renders
  // ANSI spans and splicing a <mark> through them costs more than
  // the signal is worth.
  const matches = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (q === "") return [];
    return allLines
      .filter((l) => l.text.toLowerCase().includes(q))
      .map((l) => l.seq);
  }, [allLines, query]);
  const activeSeq = matches.length > 0 ? matches[Math.min(matchIdx, matches.length - 1)] : undefined;

  const scrollToBottom = useCallback(() => {
    const el = scrollRef.current;
    if (el) el.scrollTop = el.scrollHeight;
  }, []);
  const scrollToTop = useCallback(() => {
    const el = scrollRef.current;
    if (el) el.scrollTop = 0;
  }, []);

  // Smart follow: pin to the bottom as lines stream in; a manual
  // scroll AWAY from the bottom pauses, returning to the bottom
  // resumes. The pill mirrors + toggles the state.
  useEffect(() => {
    if (follow && running) scrollToBottom();
    // logs.length is the streaming signal under the poll model.
  }, [logs.length, follow, running, scrollToBottom]);

  const onScroll = useCallback(() => {
    const el = scrollRef.current;
    if (!el || !running) return;
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
    setFollow(atBottom);
  }, [running]);

  // Permalink: honor #L<seq> on mount (deep links from PR comments
  // or incident channels land on the line).
  useEffect(() => {
    const m = /^#L(\d+)$/.exec(window.location.hash);
    if (!m) return;
    const target = document.getElementById(`L${m[1]}`);
    if (target) target.scrollIntoView({ block: "center" });
    // One-shot on mount by design.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Scroll the active search match into view.
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
    // Root owns the height budget: default cap matches the old
    // viewer (max-h-96); a fill-height consumer (the job drawer)
    // overrides via className with h-full + max-h-none and the
    // flex-1 scroll area below stretches to whatever remains —
    // no dead space under short logs, inner scroll for long ones.
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
          className="max-h-none overflow-visible"
        />
      </div>
    </div>
  );
}
