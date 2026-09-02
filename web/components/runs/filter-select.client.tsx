"use client";

import { useRouter } from "next/navigation";

// FilterSelect: a native <select> that rewrites the /runs query
// string on change. Native over a styled dropdown on purpose — the
// option lists (projects, pipeline names) can be long, and the
// browser's built-in type-ahead beats anything we'd hand-roll.
// Server components own the option lists; this only navigates.
export function FilterSelect({
  param,
  value,
  options,
  placeholder,
  context,
}: {
  param: string;
  value: string | undefined;
  options: { value: string; label: string }[];
  placeholder: string;
  context: Record<string, string | undefined>;
}) {
  const router = useRouter();

  function onChange(next: string) {
    const q = new URLSearchParams();
    for (const [k, v] of Object.entries({ ...context, [param]: next })) {
      if (v != null && v !== "") q.set(k, v);
    }
    const s = q.toString();
    router.push(s ? `/runs?${s}` : "/runs");
  }

  return (
    <select
      value={value ?? ""}
      onChange={(e) => onChange(e.target.value)}
      aria-label={placeholder}
      className="h-7 max-w-56 rounded-md border border-border bg-background px-2 text-xs text-foreground hover:bg-muted focus:outline-none focus:ring-1 focus:ring-ring"
    >
      <option value="">{placeholder}</option>
      {options.map((o) => (
        <option key={o.value} value={o.value}>
          {o.label}
        </option>
      ))}
    </select>
  );
}
