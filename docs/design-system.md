# gocdnext — design system

Living reference for the visual identity layer. Scope of this
document: color tokens + status semantics. Typography, motion,
iconography, empty-state patterns etc. come in follow-up slices.

Everything below is defined as CSS custom properties in
`web/app/globals.css`. The `@theme` block re-exports them as
Tailwind utilities (`bg-brand-500`, `text-status-success-fg`, …)
so components never read raw color values.

## Why tokens, not hex

Any component that writes `text-emerald-500` hard-couples itself
to one palette. A brand refresh, dark-mode tuning, or a
customer-specific theme becomes a grep-and-replace across the
repo. Tokens move that decision to one file.

**Rule**: components must reference design tokens only. New
Tailwind utilities on a raw color (`bg-rose-500`, …) are a lint
bug by convention, not tooling. The system token that covers the
intent already exists — if it doesn't, add it to `globals.css`
first.

## Brand palette

`--brand-50 … --brand-950` — teal-cyan at hue 195. The main brand
step is `--brand-500` in light mode; dark mode bumps to
`--brand-600`/`--brand-500` to keep chroma visible against the
deep background.

| Usage | Token |
|---|---|
| shadcn `--primary` | `--brand-600` (light) / `--brand-500` (dark) |
| Focus rings | `--brand-500` / `--brand-400` (dark) |
| Sidebar active nav | `--brand-600` / `--brand-500` (dark) |
| `chart-1` | `--brand-500` |

Brand color choice rationale: bluish-green, distinct from
Jenkins (blue-on-white), GitLab (orange), Woodpecker (green),
Drone (green), Concourse (red). Reads "CI/CD" without copying
a competitor.

## Status tokens

Each status carries three values:

- `--status-<name>-base` — the saturated color. Use for icons +
  hard-contrast accents.
- `--status-<name>-bg` — a soft tint (≈8–15% alpha in light,
  15–18% in dark). Use as badge / card / pill fill.
- `--status-<name>-fg` — text color tuned for legibility on
  `-bg`. Use on top of `-bg` fills.

Supported statuses:

| Token family | Semantic | Used for |
|---|---|---|
| `--status-success-*` | green | run/stage/job pass, webhook accepted, integration configured |
| `--status-failed-*` | red | run/stage/job fail, webhook signature rejected |
| `--status-running-*` | brand (teal-cyan) | in-flight run/stage/job — matches brand so "something is happening" feels cohesive |
| `--status-queued-*` | muted | waiting for dispatch |
| `--status-canceled-*` | muted + dashed outline | user-canceled |
| `--status-skipped-*` | muted | skipped by policy |
| `--status-warning-*` | amber | stale agent, auth-disabled banner, webhook delivery error (HTTP-level) |

## Tailwind utilities

The tokens above generate these utilities via `@theme`:

```
bg-brand-50 … bg-brand-950       (fills)
text-brand-50 … text-brand-950   (text)
border-brand-50 … border-brand-950
ring-brand-500                   (focus rings)

bg-status-success-bg
text-status-success-fg
text-status-success               (for icons)
border-status-success/30          (borders at any alpha via slash)
```

Same shape for every other status.

## Shared components on top of tokens

Raw Tailwind utilities are a valid way to consume tokens, but
most of the app should reach for these small helpers so the "pill
shape", "dot shape", etc. don't re-diverge.

### `<StatusPill tone="success">…</StatusPill>`

Canonical badge for any colored label: webhook deliveries,
integration state, artifact status, anything with a tone + text.
Maps `tone` → background/text tokens. Optional `icon` prop for a
leading glyph (`icon={Check}`).

```tsx
<StatusPill tone="success" icon={Check}>configured</StatusPill>
<StatusPill tone="warning">stale</StatusPill>
```

### `<StatusDot tone="running" />`

Tiny colored circle for list rows where a full pill would be
noisy (agents table, dashboard agents widget). Pulses
automatically on `tone="running"` — pass `pulse={false}` to
force static. Accepts `size="sm" | "md" | "lg"` and an optional
`label` for screen readers.

```tsx
<StatusDot tone="success" />
<StatusDot tone="warning" size="lg" label="stale" />
```

### Mapping domain-specific statuses

The app has several status vocabularies (run statuses, webhook
delivery statuses, agent health). Each consumer maps its own
status to a `StatusTone` at the edge — there's no central
dispatcher because the mappings are small and domain-specific:

```tsx
function agentHealthTone(state: AgentSummary["health_state"]): StatusTone {
  switch (state) {
    case "online": return "success";
    case "stale":  return "warning";
    case "idle":   return "running";
    default:       return "failed";
  }
}
```

For run/stage/job statuses, `statusTone()` in `lib/status.ts`
already does the mapping.

## Adding a new status

1. Define `--status-<name>-base / -bg / -fg` in both `:root` and
   `.dark` in `web/app/globals.css`.
2. Expose the three as `--color-status-<name>-*` inside
   `@theme`.
3. Document it in the table above.

No component-level plumbing needed — Tailwind generates the
utilities automatically.

## Non-goals (explicit)

- **Per-customer theming**: not in scope. A future slice can
  parameterize `--brand-*` from a DB-stored tenant config; for
  now the brand is fixed.
- **Type scale**: typography still uses Tailwind defaults.
  Covered by a follow-up slice.
- **Motion tokens**: animations live in Tailwind's default
  `ease-*` / `duration-*`. No semantic motion tokens yet.
- **Component tokens** (`--button-bg-hover`, …): kept out.
  shadcn's base classes are good enough; adding a third layer
  between tokens and components inflates the surface without
  clear payoff at our size.
