import { cn } from "@/lib/utils";

// YAMLView renders a YAML document with line numbers + basic token
// colouring. Purpose-built for the pipeline "yaml" tab — handles
// the subset of YAML our parser emits (block-style keys, scalar
// values, flow sequences for `stages: [a, b]`, comments, booleans
// and numbers). Not a general-purpose YAML highlighter; intentional
// so we ship zero extra deps for what's effectively a read-only
// config viewer.
//
// Colours pull from Tailwind tokens that already flip on dark mode
// via next-themes, so the tab stays legible in both themes without
// a second pass.
type Props = {
  source: string;
  className?: string;
};

type Token =
  | { kind: "text"; value: string }
  | { kind: "comment"; value: string }
  | { kind: "key"; value: string }
  | { kind: "punct"; value: string }
  | { kind: "string"; value: string }
  | { kind: "number"; value: string }
  | { kind: "bool"; value: string };

export function YAMLView({ source, className }: Props) {
  const lines = source.split("\n");
  const width = String(lines.length).length;

  return (
    <pre
      className={cn(
        "overflow-auto rounded-md border border-border bg-muted/30 p-3 font-mono text-[11px] leading-5",
        className,
      )}
    >
      <code className="block">
        {lines.map((line, i) => (
          <div key={i} className="flex">
            <span
              className="mr-3 shrink-0 select-none text-right tabular-nums text-muted-foreground/60"
              style={{ width: `${width}ch` }}
              aria-hidden
            >
              {i + 1}
            </span>
            <span className="flex-1 whitespace-pre">
              {tokenize(line).map((tok, j) => (
                <TokenSpan key={j} token={tok} />
              ))}
              {/* Preserve empty lines — whitespace-pre collapses an empty
                  span to zero height otherwise. */}
              {line === "" ? " " : null}
            </span>
          </div>
        ))}
      </code>
    </pre>
  );
}

function TokenSpan({ token }: { token: Token }) {
  switch (token.kind) {
    case "comment":
      return (
        <span className="italic text-muted-foreground/80">{token.value}</span>
      );
    case "key":
      return (
        <span className="text-sky-700 dark:text-sky-300">{token.value}</span>
      );
    case "punct":
      return <span className="text-muted-foreground">{token.value}</span>;
    case "string":
      return (
        <span className="text-emerald-700 dark:text-emerald-300">
          {token.value}
        </span>
      );
    case "number":
      return (
        <span className="text-amber-700 dark:text-amber-300">
          {token.value}
        </span>
      );
    case "bool":
      return (
        <span className="text-purple-700 dark:text-purple-300">
          {token.value}
        </span>
      );
    case "text":
      return <>{token.value}</>;
  }
}

// tokenize consumes one line and emits spans. Block-style keys
// (`foo:` at start-of-statement), list dashes, inline quoted
// strings, booleans and numbers get their own classes; everything
// else passes through as plain text.
function tokenize(line: string): Token[] {
  const comment = line.match(/^(\s*)(#.*)$/);
  if (comment) {
    return [
      { kind: "text", value: comment[1] ?? "" },
      { kind: "comment", value: comment[2] ?? "" },
    ];
  }

  const out: Token[] = [];
  // Leading indent (possibly with list dash).
  const indent = line.match(/^(\s*(?:-\s+)?)/);
  let cursor = 0;
  if (indent) {
    cursor = indent[0].length;
    if (cursor > 0) {
      // Split the indent so the dash gets its own punct colour.
      const dashIdx = indent[0].indexOf("-");
      if (dashIdx >= 0) {
        out.push({ kind: "text", value: indent[0].slice(0, dashIdx) });
        out.push({ kind: "punct", value: "-" });
        out.push({ kind: "text", value: indent[0].slice(dashIdx + 1) });
      } else {
        out.push({ kind: "text", value: indent[0] });
      }
    }
  }

  const rest = line.slice(cursor);

  // Key at the start of the remaining text: `name:` or `jobs:`.
  // The key ends at the first colon followed by space/EOL.
  const keyMatch = rest.match(/^([A-Za-z_][\w.-]*)\s*:(\s|$)/);
  if (keyMatch) {
    out.push({ kind: "key", value: keyMatch[1] ?? "" });
    out.push({ kind: "punct", value: ":" });
    // Everything after the colon — could be a value.
    const valueStart = (keyMatch[1]?.length ?? 0) + 1;
    const tail = rest.slice(valueStart);
    out.push(...tokenizeValue(tail));
    return out;
  }

  // Not a key — tokenize as value on its own (e.g. a list item value).
  out.push(...tokenizeValue(rest));
  return out;
}

function tokenizeValue(v: string): Token[] {
  if (v === "") return [];

  const out: Token[] = [];
  // Quoted string covers "go test -race ./..." style script lines
  // where the whole value is wrapped.
  const quoted = v.match(/^(\s*)(["'])(.*?)\2(.*)$/);
  if (quoted) {
    out.push({ kind: "text", value: quoted[1] ?? "" });
    out.push({ kind: "string", value: `${quoted[2]}${quoted[3]}${quoted[2]}` });
    out.push(...tokenizeValue(quoted[4] ?? ""));
    return out;
  }

  // Boolean / null literal as the whole value (YAML truthy atoms).
  const bool = v.match(/^(\s*)(true|false|null|~|yes|no)(\s*)$/);
  if (bool) {
    out.push({ kind: "text", value: bool[1] ?? "" });
    out.push({ kind: "bool", value: bool[2] ?? "" });
    out.push({ kind: "text", value: bool[3] ?? "" });
    return out;
  }

  // Pure number, optionally leading sign / decimal.
  const num = v.match(/^(\s*)(-?\d+(?:\.\d+)?)(\s*)$/);
  if (num) {
    out.push({ kind: "text", value: num[1] ?? "" });
    out.push({ kind: "number", value: num[2] ?? "" });
    out.push({ kind: "text", value: num[3] ?? "" });
    return out;
  }

  // Flow sequence — `[build, test, deploy]`. Colour the brackets +
  // commas as punct so the value bits pop.
  if (/^\s*\[.*\]\s*$/.test(v)) {
    let i = 0;
    while (i < v.length) {
      const ch = v[i] ?? "";
      if (ch === "[" || ch === "]" || ch === ",") {
        out.push({ kind: "punct", value: ch });
        i++;
        continue;
      }
      // Consume plain-ish content up to the next bracket/comma.
      let j = i;
      while (j < v.length && !"[],".includes(v[j] ?? "")) j++;
      const slice = v.slice(i, j);
      if (slice.trim() === "") {
        out.push({ kind: "text", value: slice });
      } else {
        out.push({ kind: "string", value: slice });
      }
      i = j;
    }
    return out;
  }

  return [{ kind: "text", value: v }];
}
