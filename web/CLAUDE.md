# gocdnext — Frontend (web/)

Regras específicas do frontend. Vale em conjunto com o `CLAUDE.md` da raiz (TDD, ~400 linhas/arquivo, Conventional Commits, etc.).

Stack: **Next.js 15 (App Router) + React 19 + TypeScript + Tailwind + shadcn/ui**.

## Regras não-negociáveis (reforço do root)

- **shadcn para UI.** Todo componente visual sai de `shadcn/ui`. Não criar Button, Dialog, Input, Form, Table próprios quando o equivalente existe. Customização via `className` (com `cn()`) e variantes do CVA.
- **~400 linhas por arquivo** (`.ts`, `.tsx`). Quebrar em subcomponentes ou hooks quando passar.
- **TDD**: teste de componente/action antes do código.

## TypeScript

- **`strict: true`** + `noUncheckedIndexedAccess`, `noImplicitOverride`, `noFallthroughCasesInSwitch`.
- **Nada de `any`**; usar `unknown` + narrow. `as` só em último caso e com comentário do porquê.
- **Tipos inferidos de Zod**: `type Foo = z.infer<typeof fooSchema>`. Evitar declarar tipo paralelo ao schema.
- **Path alias** via `@/`: `@/components/ui/button`, `@/lib/utils`, `@/server/actions/pipelines`.
- **`satisfies`** em vez de cast quando possível.

## Next.js (App Router)

- **RSC por padrão.** `"use client"` só quando há: estado local, evento de UI, hook de browser (`useEffect`, `useState`, `useRef`), Context, ou lib que pede DOM.
- **Data fetching em RSC** com `fetch` + `cache: 'force-cache' | 'no-store'` explícito. Evitar SWR/React Query no servidor.
- **Server Actions** para toda mutation:
  - Arquivo `web/src/server/actions/<domain>.ts` com `"use server"` no topo.
  - Validação com Zod **antes** da lógica.
  - Retornar `{ ok: true, data }` ou `{ ok: false, error }` — não lançar para o client.
  - Revalidar com `revalidatePath` / `revalidateTag` após mutation bem-sucedida.
- **Nunca criar `route.ts` (API route) quando Server Action resolve.** API route só para webhooks externos, endpoints públicos, streaming não-trivial.
- **Streaming**: `Suspense` + `loading.tsx`. Skeleton via shadcn.
- **Error boundary**: `error.tsx` em cada rota com efeito colateral.
- **Metadata**: exportar `metadata` ou `generateMetadata` em cada `page.tsx`.

## Estrutura de pastas

```
web/src/
  app/
    (dashboard)/           # rotas autenticadas
      pipelines/
        page.tsx           # lista (RSC)
        [id]/
          page.tsx
          runs/page.tsx
      layout.tsx
    api/                   # só para webhooks/endpoints externos
    layout.tsx
    page.tsx
  components/
    ui/                    # shadcn — instalado via CLI, não editar à mão sem motivo
    pipelines/             # componentes de domínio
    vsm/
    shared/                # genéricos do app
  lib/
    utils.ts               # cn(), helpers puros
    env.ts                 # Zod-validated process.env
    grpc-client.ts         # cliente gRPC para server actions
  server/
    actions/               # Server Actions por domínio
    queries/               # funções que rodam em RSC
  hooks/                   # client hooks (use*)
  types/                   # tipos compartilhados
```

- **`app/`** = roteamento e composição.
- **`components/`** = apresentação. Não importa de `server/`.
- **`server/`** = lógica server-side. Pode importar de `lib/`.
- **`lib/`** = puro, sem I/O (exceto `grpc-client.ts`).

## Componentes

- **Server Component por default.** Se precisar virar client, extrair só o pedaço interativo em componente separado (`FooButton.client.tsx` dentro do mesmo dir).
- **Props tipadas explicitamente**, nunca `React.FC`:
  ```tsx
  type Props = { id: string; onSelect?: (id: string) => void };
  export function PipelineCard({ id, onSelect }: Props) { ... }
  ```
- **Composição > props gigantes.** Se um componente tem >7 props, provavelmente é dois.
- **`children` > render props** em RSC.
- **Evento `onX` no client**, callback `action` quando passa Server Action:
  ```tsx
  <form action={createPipeline}> ... </form>
  ```

## shadcn/ui

- **Instalação via CLI**: `pnpm dlx shadcn@latest add <component>`. Saída em `components/ui/`.
- **Atualização controlada**: quando precisar rebaixar um componente do shadcn, PR separado só com esse update.
- **Customização**:
  - Novos variants via CVA dentro do próprio arquivo do componente.
  - Cores/radius via tokens CSS em `app/globals.css`, nunca hardcoded no componente.
- **Forms**: `@/components/ui/form` + `react-hook-form` + `@hookform/resolvers/zod`. Padrão obrigatório em form com validação.

## Tailwind

- **Mobile-first**: classes sem prefixo = mobile; `sm:`, `md:`, `lg:` escalam para cima.
- **Tokens do design system** (`bg-background`, `text-foreground`, `border-border`) em vez de cores cruas (`bg-white`, `text-gray-900`).
- **`cn()`** para composição condicional. Nunca `${a} ${b}` manual.
- **Classe arbitrária `[valor]`** só quando o token não existe; não virar hábito.
- **Não usar `@apply`** exceto em tokens globais em `globals.css`.

## Validação e dados

- **Zod em todo boundary**:
  - Form input (via `zodResolver`).
  - Server Action input (parse antes de tocar server-side).
  - Webhook payload em `route.ts`.
  - `env.ts` valida `process.env` no boot.
- **Tipos de domínio vêm do proto** (TS gerado via `buf`) quando a entidade é compartilhada server↔client. Não redeclarar.

## Estado e dados no client

- **Minimizar estado no client.** RSC + Server Actions cobrem a maioria dos casos.
- Quando precisar: `useState` para local, `useReducer` para multi-campo. **Não introduzir Zustand/Jotai sem motivo claro.**
- **SWR/React Query** apenas para casos de polling/revalidação no client (ex: logs SSE, status de job em tempo real).
- **`useOptimistic`** para UX de mutation rápida.

## Acessibilidade

- **Todos os componentes interativos têm `aria-*` quando necessário.** shadcn já vem ok, manter.
- **Foco visível**: nunca remover `outline` sem substituir por `focus-visible:ring`.
- **Labels explícitos** em inputs, sem placeholder-as-label.
- **Navegação por teclado**: testar Tab/Enter/Esc em modal, combobox, menu.

## Testes

- **Vitest + React Testing Library** para componente e hook.
- **Playwright** para e2e (fluxo de login, criar pipeline, ver run, ver VSM).
- **MSW** para mockar fetch em teste de componente. Server Actions testadas diretamente (função pura chamável).
- **Teste por comportamento, não por implementação**: consultar por `role`, `label`, `text`; evitar `data-testid` exceto em elemento sem semântica.
- **Snapshot só para estruturas estáveis** (ex: VSM renderizado com fixture). Nunca snapshot de componente em evolução.

## Performance

- **`next/image`** para toda imagem. Width/height obrigatórios.
- **`next/font`** para fonte. Sem `<link>` no head.
- **Dynamic import (`next/dynamic`)** para componente client pesado (gráficos, editor Monaco).
- **`Suspense` granular** em seção de página com fetch lento, não no root do layout.
- **Evitar `"use client"` no topo de árvore** — empurra boundary o mais fundo possível.

## Lint e formatter

- **Biome** (toolchain único). Config em `biome.json`.
- **TypeScript check no CI**: `tsc --noEmit`. Erro = CI vermelho.
- **Pre-commit**: `biome check --apply` + `tsc --noEmit` nos arquivos afetados.

## Antes de abrir PR (específico do web)

- `pnpm build` passa sem warning.
- `pnpm test` verde (unit + component).
- `pnpm exec playwright test` verde quando mexeu em fluxo coberto por e2e.
- Lighthouse local: LCP < 2.5s na rota tocada, sem regressão de a11y.
- Componente novo usa shadcn quando aplicável.
