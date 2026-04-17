# gocdnext — Guia de Desenvolvimento

Regras de engenharia para este repositório. Válido para todo código que entrar em `server/`, `agent/`, `cli/`, `plugins/`, `web/`, `proto/`.

Decisões arquiteturais já fechadas (stack, layout `.gocdnext/`, LISTEN/NOTIFY, sqlc, gRPC bidi, plugin-container) vivem em `docs/` e não são rediscutidas sem razão técnica nova.

## Regras não-negociáveis

- **TDD sempre.** Teste vermelho → código mínimo → verde → refactor. Nenhum PR sem teste novo ou teste cobrindo o caminho alterado.
- **~400 linhas por arquivo.** Passou disso, quebrar. Vale para `.go`, `.ts`, `.tsx`, `.sql`. Testes podem exceder se for um único suite coeso.
- **shadcn para UI.** Todo componente visual no `web/` sai de shadcn/ui. Não escrever Button, Dialog, Input próprios quando o equivalente shadcn existe. Customização via `className` e variantes.

## Go (server, agent, cli, plugins)

- **Lint**: `golangci-lint` com `.golangci.yml` na raiz. Presets ativos: `errcheck`, `govet`, `staticcheck`, `gosec`, `revive`, `gocyclo`, `ineffassign`, `unused`. CI falha no primeiro warning.
- **Race detector obrigatório**: `go test -race ./...` no CI. Não desativar localmente.
- **Context sempre 1º argumento** em função que faz I/O, chama gRPC, ou toca banco. `context.Background()` só em `main` e testes.
- **Erros embrulhados com `%w`**: `fmt.Errorf("parse pipeline %s: %w", name, err)`. Assertion com `errors.Is` / `errors.As`.
- **Logging estruturado** com `slog`. Nada de `fmt.Println`, `log.Printf` em código de produção. Campos consistentes: `pipeline`, `job`, `agent_id`, `run_id`.
- **Table-driven tests** como padrão:
  ```go
  tests := []struct{ name string; in X; want Y }{...}
  for _, tt := range tests { t.Run(tt.name, func(t *testing.T) {...}) }
  ```
- **Integração com Postgres usa `testcontainers-go`**, nunca mock. Se o teste precisa de DB, sobe container real.
- **Nomes de pacote**: lowercase único, sem underscore, sem plural. `pipeline`, não `pipelines` ou `pipeline_parser`.
- **sqlc gera em `internal/db/`**. Código gerado nunca editado à mão.

## Frontend (web/)

Regras específicas do frontend (Next.js 15, RSC, Server Actions, shadcn, Tailwind, Zod, Biome, testes) estão em [web/CLAUDE.md](web/CLAUDE.md). Claude Code carrega hierarquicamente — ao trabalhar em `web/`, ambos os arquivos valem.

## Proto / contratos gRPC

- **`buf`** gerencia proto. `buf.yaml` + `buf.gen.yaml` na raiz.
- **Lint no CI**: `buf lint` e `buf breaking --against '.git#branch=main'`. Breaking change exige bump de versão do pacote (`v1` → `v2`).
- **Código gerado nunca editado.** Regenerar com `buf generate`. Saída em `proto/gen/go` e `proto/gen/ts`.
- **Contratos vivem em `proto/gocdnext/v1/`**. Novo serviço = novo arquivo.

## Git, commits e CI

- **Conventional Commits** obrigatório: `feat(scope):`, `fix(scope):`, `chore:`, `docs:`, `test:`, `refactor:`. Scope opcional mas recomendado.
- **Pre-commit hook** via lefthook. Roda: `gofmt`, `golangci-lint run --fast`, `buf lint`, `tsc --noEmit`, testes rápidos afetados.
- **PR pequeno e focado.** Um PR = uma feature/fix. Refactor grande em PR separado do feature.
- **CI GitHub Actions**: lint → build → testes unitários → testes de integração (com containers) → testes e2e (quando existirem).
- **Migrations**: goose, forward-only. Não criar `.down.sql` que rode em produção. Rollback = nova migration corretiva.
- **Secrets**: `.env.example` commitado, `.env` no `.gitignore`. Nenhuma credencial em YAML de pipeline, Dockerfile ou código.

## Dependências

- **Renovate/Dependabot** atualiza deps semanalmente. Humano revisa e merge.
- **Dep nova exige justificativa no PR**: "por que não dá com stdlib ou com o que já tem?". Evitar sprawl de libs.
- **Fixar versão major** em `go.mod` e `package.json`. Minor/patch livres para atualizar.

## Observabilidade (desde Fase 1)

- **OpenTelemetry traces** no server e agent desde o primeiro endpoint/stream. Retrofitar depois é caro.
  - Spans nomeados: `pipeline.parse`, `job.schedule`, `agent.stream.recv`, `webhook.receive`.
  - Propagação via contexto gRPC e HTTP headers.
- **Métricas Prometheus** em `/metrics`:
  - Jobs: `gocdnext_jobs_scheduled_total`, `gocdnext_jobs_running`, `gocdnext_job_duration_seconds`.
  - Fila: `gocdnext_queue_depth{stage}`.
  - gRPC: latência, RPS, erros por método.
- **Logs correlacionados** com trace_id e span_id via `slog` handler OTel-aware.

## Convenções de diretórios

```
server/
  cmd/gocdnext-server/   # main
  internal/
    api/      # HTTP handlers + Server Actions-facing
    grpc/     # gRPC services
    db/       # sqlc generated
    domain/   # tipos de negócio puros
    pipeline/ # parser, validator, scheduler
  migrations/ # goose

agent/
  cmd/gocdnext-agent/
  internal/
    runtime/  # docker/k8s executor
    stream/   # gRPC client
    plugin/   # plugin runner

web/
  src/
    app/              # Next.js App Router
    components/ui/    # shadcn
    components/       # app components
    lib/              # utils
    server/           # Server Actions

proto/gocdnext/v1/
plugins/<name>/
```

- `internal/` é privado ao módulo. Nada de import cruzado entre `internal/` de módulos diferentes — usar proto ou pacote público.

## Antes de abrir PR

- Testes verdes localmente, com race detector.
- Lint limpo (`make lint`).
- `buf lint` e `buf breaking` ok.
- Sem `TODO` órfão (se ficar, linkar issue).
- Sem arquivo > 400 linhas (teste aceito).
- Commit message no padrão Conventional Commits.
