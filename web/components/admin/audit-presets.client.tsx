"use client";

import { Button } from "@/components/ui/button";

// AuditPresets renders a quick-pick row that writes common date
// windows into the form's from/to inputs and immediately submits.
// Uses DOM queries instead of controlled state because the date
// inputs are uncontrolled (GET form) — we just flip their values
// and requestSubmit the containing form.
//
// "Today" / "7d" / "30d" are the three windows operators actually
// scan for when triaging. Anything longer is an ad-hoc analysis
// where manual date entry makes more sense.
type Preset = { label: string; days: number };

const presets: Preset[] = [
  { label: "Today", days: 0 },
  { label: "7d", days: 7 },
  { label: "30d", days: 30 },
];

export function AuditPresets() {
  const apply = (days: number) => {
    const now = new Date();
    // Half-open window: from = (today - days), to = tomorrow.
    // Matches the server's [from, to) semantics so "Today"
    // yields the full current day regardless of timezone quirks.
    const to = new Date(now);
    to.setDate(to.getDate() + 1);
    const from = new Date(now);
    from.setDate(from.getDate() - days);

    const fromInput = document.getElementById("audit-from") as
      | HTMLInputElement
      | null;
    const toInput = document.getElementById("audit-to") as HTMLInputElement | null;
    if (!fromInput || !toInput) return;

    fromInput.value = toISODate(from);
    toInput.value = toISODate(to);
    fromInput.form?.requestSubmit();
  };

  const clear = () => {
    const fromInput = document.getElementById("audit-from") as
      | HTMLInputElement
      | null;
    const toInput = document.getElementById("audit-to") as HTMLInputElement | null;
    if (!fromInput || !toInput) return;
    fromInput.value = "";
    toInput.value = "";
    fromInput.form?.requestSubmit();
  };

  return (
    <div className="flex items-end gap-1">
      {presets.map((p) => (
        <Button
          key={p.label}
          type="button"
          variant="outline"
          size="sm"
          onClick={() => apply(p.days)}
          className="h-8 px-2 text-xs"
        >
          {p.label}
        </Button>
      ))}
      <Button
        type="button"
        variant="ghost"
        size="sm"
        onClick={clear}
        className="h-8 px-2 text-xs text-muted-foreground"
      >
        clear
      </Button>
    </div>
  );
}

function toISODate(d: Date): string {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${y}-${m}-${day}`;
}
