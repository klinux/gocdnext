# Roadmap

## Agora (2026-04-20)

**Fase E2 — artefatos ✅ (fechada).** 9 commits:

- **E2a.1** storage interface + filesystem backend + handler assinado
- **E2a.2** upload end-to-end (RPC + agent tar + JobResult reconcile;
  MinIO sai do docker-compose)
- **E2b.1** S3 backend (AWS SDK v2 + LocalStack integration)
- **E2b.2** GCS backend (V4 signing, fake-gcs-server parcial)
- **UI Artifacts tab** na página de run (polling + download signed URL)
- **E2c** download intra-run (`needs_artifacts:` + untar + sha check)
- **E2d.1** fanout cross-run (`from_pipeline:` via cause_detail.upstream)
- **E2d.2.a** sweeper TTL + idempotent retry
- **E2d.2.b** keep-last + quota de projeto + quota global

**VSM ✅ (2 commits).** Endpoint `/api/v1/projects/{slug}/vsm` retorna
nodes (pipelines + latest run + git materials) + edges (upstream).
Página `/projects/{slug}/vsm` com `@xyflow/react`, layout por depth
(upstream roots à esquerda), click no counter leva ao run detail.

**PR.1 ✅ (1 commit).** Webhook aceita `pull_request` (opened /
synchronize / reopened). Match por (repo, base_ref) quando material
lista `pull_request` em `on:`. Run criada com branch = head ref,
cause=`pull_request`, cause_detail carrega metadata. UI mostra banner
com #number, author, head→base, short SHA. Closed/merged ignorados.

**GitHub App — auto-register webhook + Checks API ✅ (3 commits):**

- **APP.1** ✅ `github.AppClient`: JWT RS256 hand-rolled sobre stdlib
  (sem lib JWT adicional), installation token cache com TTL, PKCS#1
  e PKCS#8 PEM, `NewAppClientFromEnv` retorna nil quando App não
  configurada.
- **APP.2** ✅ auto-register webhook: `gocdnext apply` cria hook via
  `POST /repos/{o}/{r}/hooks` quando material git tem
  `auto_register_webhook: true`; idempotente (checa hooks existentes
  por prefixo de URL); best-effort (erro em um material não falha o
  apply); status por material na resposta: `registered`,
  `already_exists`, `skipped_no_install`, `failed`.
- **APP.3** ✅ Checks API: reporter cria check_run em `in_progress`
  no momento da criação da run (webhook push / pull_request),
  atualiza pra `completed` com conclusão
  `success|failure|cancelled|neutral` quando a run termina. Prefere
  PR head SHA vem do `cause_detail` sobre material revision. Link
  `run_id → check_run_id` persiste na nova tabela
  `github_check_runs`.

Com isso a **dogfood-readiness estrutural fechou**. Fase 3 (Helm +
K8s agent + pipelines reais) é o próximo capítulo.

## Fase 0 — fundação ✅

- [x] Monorepo layout, `go.work`, `go.mod` por módulo
- [x] Contratos proto (agent, pipeline, common) + `buf generate`
- [x] Schema SQL + migrations forward-only via goose
- [x] Tipos de domínio + parser YAML (com testes)
- [x] docker-compose dev stack (postgres + MinIO — MinIO sai no E2a.2)
- [x] Dockerfiles (server, agent)
- [x] Makefile + `make proto`
- [ ] CI no próprio gocdnext (pendente; dogfood depois)

## Fase 1 — MVP pipeline ✅

- [x] Webhook GitHub + validação HMAC (e persistência de delivery)
- [x] Persistência de `modifications` a partir do webhook
- [x] Scheduler: loop `LISTEN run_queued`, cria runs, despacha jobs
- [x] Agent: gRPC `Register`/`Connect`, checkout git, script em container
- [x] Agent: streaming de `LogLine` pro server
- [x] Web UI: lista de projetos/runs, live log via TanStack polling
- [x] CLI `gocdnext` com `validate`, `secret set/list/rm`

Critério de saída atingido: push → pipeline roda → logs visíveis no UI.

## Fase 2 — o diferencial (parcial)

- [x] Material `upstream:` → fanout paralelo em runs downstream
- [x] Expansão de `parallel.matrix`
- [x] Execução de step plugin (contrato Woodpecker-like)
- [x] Avaliação de `rules` (`if` / `changes` / `when`)
- [x] Detecção de drift de config (`ConfigFetcher` GitHub API)
- [x] Tag matching: agente com `tags: [docker]` só recebe jobs com tag
- [x] Secrets: store AES-GCM, `Resolver` interface, UI shadcn, mask em log
- [x] Reaper: reclaim de jobs órfãos, retry contados por tentativa
- [x] **Artefatos** — Fase E2 fechada (acima)
- [x] Endpoint VSM + visualização `@xyflow/react`
- [x] Suporte nativo a PR (trigger + UI; Checks API vai na próxima)
- [x] Auto-register de webhook via GitHub App flow (APP.2)
- [x] GitHub Checks API reportando status do run (APP.3)

## Fase 3 — validação interna

- [ ] Helm chart pra K8s
- [ ] Agente K8s-native (jobs rodam como `Job`/`Pod`)
- [ ] Secrets via K8s Secret refs (adapter do `Resolver`)
- [x] Upload de artefato — coberto pela Fase E2 acima
- [ ] Rodar 3–5 pipelines reais da própria org
- [ ] Coletar feedback, iterar UX

Critério de saída: gocdnext rodando ≥1 pipeline *produção-crítico* por 2
semanas seguidas.

## Gate: abrir ou continuar interno?

Entradas da decisão:
- Usuários internos preferiram ao que tinham antes?
- VSM + fanout se mostrou diferencial real?
- Temos bandwidth pra manter projeto público?

## Parking lot (futuro)

- Multi-tenant + RBAC
- Marketplace de plugins / modelo de trust
- Approval gates (stage manual)
- Cron scheduling
- Config-as-code em repo separado (estilo GoCD config-repo)
- Paridade GitLab + Bitbucket (webhooks e config-repo)
- ClickHouse / Loki pra logs em escala
- Scheduler distribuído (HA multi-server)
- **Test Reports** — parser JUnit/Cobertura + aba Tests no UI + histórico
  de flakiness. Design própria quando for a vez
  (`docs/test-reports-design.md`).
- Cache step (`cache:` no YAML) — semântica key-addressed/LRU, separada de
  artifacts. Ver discussão na seção 6 do `artifacts-design.md`.
- Adapters de secret externos (Vault, AWS Secrets Manager, GCP Secret
  Manager) — `Resolver` já está pronto pra receber, implementação quando
  alguém pedir.
