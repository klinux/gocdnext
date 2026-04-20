# Roadmap

## Agora (2026-04-19)

Em andamento: **Fase E2 — artefatos**. Design fechado em
[artifacts-design.md](artifacts-design.md).

- **E2a.1 — storage + filesystem backend + handler assinado.** ✅
  `artifacts.Store` interface, filesystem default, HMAC signed URLs,
  handler HTTP `/artifacts/{token}`, tabela `artifacts` com campos de
  retenção.
- **E2a.2 — upload end-to-end.** ✅ Proto
  `RequestArtifactUpload`, RPC server-side, agent tar+gz + PUT,
  confirmação via HEAD+JobResult, MinIO fora do docker-compose.
- **E2b.1 — S3 backend.** ✅ AWS SDK v2, endpoint-overridable (R2,
  Tigris, LocalStack). Integração via LocalStack.
- **E2b.2 — GCS backend.** ✅ `cloud.google.com/go/storage`, V4
  signing com JSON service-account key. Integração parcial via
  fake-gcs-server (Put/Head/Delete; Get depende de download URL
  externa que o fake não resolve bem — coberto por teste direto em
  prod).
- **E2c — download intra-run.** ✅ `needs_artifacts:` no YAML,
  scheduler resolve + emite GETs assinados no `JobAssignment`,
  agent baixa+verifica sha+untara antes das tasks.
- **E2d.1 — fanout cross-run.** ✅ `from_pipeline: build-core` puxa
  artefatos da run upstream que triggou. `examples/fanout/` deixa
  de ser teatro.
- **E2d.2 — sweeper de retenção.** ✅ 4 camadas (TTL, keep-last
  por pipeline, quota de projeto soft, quota global hard), ordem
  determinística, idempotente, pinned-at skipa tudo.

**Artefato fechado.** Volta pra mesa **dogfood-readiness**: VSM +
xyflow (diferencial visual), PR support, auto-register webhook via
GitHub App.

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
- [ ] **Artefatos** — em andamento (Fase E2 acima)
- [ ] Endpoint VSM + visualização `@xyflow/react`
- [ ] Suporte nativo a PR (branch-per-run, counters isolados)
- [ ] Auto-register de webhook via GitHub App flow (schema já tem a flag)

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
