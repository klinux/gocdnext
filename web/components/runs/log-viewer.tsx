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
      {logs.map((line) => (
        <div
          key={line.seq}
          data-stream={line.stream}
          className={cn(
            "grid grid-cols-[3rem_1fr] gap-3",
            line.stream === "stderr" ? "text-destructive" : "text-foreground",
          )}
        >
          <span className="select-none text-right text-muted-foreground">
            {line.seq}
          </span>
          <span className="whitespace-pre-wrap break-all">{line.text}</span>
        </div>
      ))}
    </pre>
  );
}
