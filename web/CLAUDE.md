# gocdnext — Frontend (web/)

Frontend-specific rules. Applies together with the root `CLAUDE.md` (TDD, ~400 lines/file, Conventional Commits, etc.).

Stack: **Next.js 15 (App Router) + React 19 + TypeScript + Tailwind + shadcn/ui**.

## Non-negotiable rules (root reinforcement)

- **shadcn for UI.** Every visual component comes from `shadcn/ui`. Don't roll your own Button, Dialog, Input, Form, Table when an equivalent exists. Customise via `className` (with `cn()`) and CVA variants.
- **~400 lines per file** (`.ts`, `.tsx`). Past that, split into subcomponents or hooks.
- **TDD**: component/action test before the code.

## TypeScript

- **`strict: true`** + `noUncheckedIndexedAccess`, `noImplicitOverride`, `noFallthroughCasesInSwitch`.
- **No `any`**; use `unknown` + narrow. `as` only as a last resort, with a comment explaining why.
- **Types inferred from Zod**: `type Foo = z.infer<typeof fooSchema>`. Avoid declaring a parallel type alongside the schema.
- **Path alias** via `@/`: `@/components/ui/button`, `@/lib/utils`, `@/server/actions/pipelines`.
- **`satisfies`** instead of cast whenever possible.

## Next.js (App Router)

- **RSC by default.** `"use client"` only when there's: local state, a UI event, a browser hook (`useEffect`, `useState`, `useRef`), Context, or a lib that needs DOM.
- **Data fetching in RSC** with `fetch` + explicit `cache: 'force-cache' | 'no-store'`. Avoid SWR/React Query on the server.
- **Server Actions** for every mutation:
  - File `web/src/server/actions/<domain>.ts` with `"use server"` at the top.
  - Zod validation **before** the logic.
  - Return `{ ok: true, data }` or `{ ok: false, error }` — never throw to the client.
  - Revalidate with `revalidatePath` / `revalidateTag` after a successful mutation.
- **Never create a `route.ts` (API route) when a Server Action solves it.** API routes only for external webhooks, public endpoints, non-trivial streaming.
- **Streaming**: `Suspense` + `loading.tsx`. Skeleton via shadcn.
- **Error boundary**: an `error.tsx` in every route with side effects.
- **Metadata**: export `metadata` or `generateMetadata` in every `page.tsx`.

## Folder structure

```
web/src/
  app/
    (dashboard)/           # authenticated routes
      pipelines/
        page.tsx           # list (RSC)
        [id]/
          page.tsx
          runs/page.tsx
      layout.tsx
    api/                   # only for webhooks/external endpoints
    layout.tsx
    page.tsx
  components/
    ui/                    # shadcn — installed via CLI, don't hand-edit without reason
    pipelines/             # domain components
    vsm/
    shared/                # app-wide generics
  lib/
    utils.ts               # cn(), pure helpers
    env.ts                 # Zod-validated process.env
    grpc-client.ts         # gRPC client used by server actions
  server/
    actions/               # Server Actions by domain
    queries/               # functions that run inside RSC
  hooks/                   # client hooks (use*)
  types/                   # shared types
```

- **`app/`** = routing and composition.
- **`components/`** = presentation. Doesn't import from `server/`.
- **`server/`** = server-side logic. May import from `lib/`.
- **`lib/`** = pure, no I/O (except `grpc-client.ts`).

## Components

- **Server Component by default.** When it must become a client component, extract just the interactive bit into a separate component (`FooButton.client.tsx` in the same dir).
- **Explicitly typed props**, never `React.FC`:
  ```tsx
  type Props = { id: string; onSelect?: (id: string) => void };
  export function PipelineCard({ id, onSelect }: Props) { ... }
  ```
- **Composition > giant prop bags.** If a component has >7 props, it's probably two.
- **`children` > render props** in RSC.
- **`onX` event on the client**; `action` callback when passing a Server Action:
  ```tsx
  <form action={createPipeline}> ... </form>
  ```

## shadcn/ui

- **Install via CLI**: `pnpm dlx shadcn@latest add <component>`. Output lands in `components/ui/`.
- **Controlled updates**: when you need to bump a shadcn component, put it in its own PR.
- **Customisation**:
  - New variants via CVA inside the component's own file.
  - Colours/radius via CSS tokens in `app/globals.css`, never hardcoded in the component.
- **Forms**: `@/components/ui/form` + `react-hook-form` + `@hookform/resolvers/zod`. Mandatory pattern for any form with validation.

## Tailwind

- **Mobile-first**: classes without a prefix = mobile; `sm:`, `md:`, `lg:` scale up.
- **Design-system tokens** (`bg-background`, `text-foreground`, `border-border`) instead of raw colours (`bg-white`, `text-gray-900`).
- **`cn()`** for conditional composition. Never manual `${a} ${b}`.
- **Arbitrary class `[value]`** only when the token doesn't exist; don't make it a habit.
- **No `@apply`** except for global tokens in `globals.css`.

## Validation and data

- **Zod at every boundary**:
  - Form input (via `zodResolver`).
  - Server Action input (parse before touching server-side logic).
  - Webhook payload in `route.ts`.
  - `env.ts` validates `process.env` at boot.
- **Domain types come from proto** (TS generated via `buf`) when the entity is shared server↔client. Don't redeclare.

## Client state and data

- **Minimise client state.** RSC + Server Actions cover most cases.
- When needed: `useState` for local state, `useReducer` for multi-field. **Don't reach for Zustand/Jotai without a clear reason.**
- **SWR/React Query** only for client polling/revalidation cases (e.g., log SSE, live job status).
- **`useOptimistic`** for fast-mutation UX.

## Accessibility

- **Every interactive component has the right `aria-*`.** shadcn comes correct by default; keep it that way.
- **Visible focus**: never remove `outline` without replacing with `focus-visible:ring`.
- **Explicit labels** on inputs — no placeholder-as-label.
- **Keyboard navigation**: test Tab/Enter/Esc on every modal, combobox, menu.

## Testing

- **Vitest + React Testing Library** for components and hooks.
- **Playwright** for e2e (login flow, create pipeline, view run, view VSM).
- **MSW** to mock `fetch` in component tests. Server Actions are tested directly (they're pure callable functions).
- **Test by behaviour, not implementation**: query by `role`, `label`, `text`; avoid `data-testid` except on elements without semantics.
- **Snapshot only stable structures** (e.g., VSM rendered against a fixture). Never snapshot a component in flux.

## Performance

- **`next/image`** for every image. Width/height mandatory.
- **`next/font`** for fonts. No `<link>` in the head.
- **Dynamic import (`next/dynamic`)** for heavy client components (charts, Monaco editor).
- **Granular `Suspense`** around a page section with a slow fetch, not at the root of the layout.
- **Avoid `"use client"` at the top of the tree** — push the boundary as deep as possible.

## Lint and formatter

- **Biome** (single toolchain). Config in `biome.json`.
- **TypeScript check in CI**: `tsc --noEmit`. Errors = red CI.
- **Pre-commit**: `biome check --apply` + `tsc --noEmit` on the affected files.

## Before opening a PR (web-specific)

- `pnpm build` passes without warnings.
- `pnpm test` green (unit + component).
- `pnpm exec playwright test` green when you touched a flow covered by e2e.
- Local Lighthouse: LCP < 2.5s on the touched route, no a11y regression.
- New component uses shadcn when applicable.
