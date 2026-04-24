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

---

## Fase 3 — K8s + Helm ✅ (4 commits)

- **F3.1** ✅ `agent.engine` interface + Shell impl (refactor sem
  mudança de comportamento; destrava F3.2).
- **F3.2** ✅ `engine.Kubernetes`: cria Pod por task, streama logs
  via GetLogs(Follow), cleanup configurável. Workspace via PVC
  compartilhado entre agent Pod e job Pod.
- **F3.3** ✅ Helm chart: server + agent + web + RBAC + PVCs;
  condicionais por backend de artefato, engine do agent, dev
  postgres. `helm lint` limpo, 14 objetos renderizados no default.
- **F3.4** ✅ `secrets.KubernetesResolver`: projeto vira Secret
  `gocdnext-secrets-{slug}`; chart adiciona Role `get` em Secrets
  quando `secrets.backend=kubernetes`.

Próximo item é operacional: **rodar pipelines reais**. Instalar o
App de verdade num repo, instalar o chart num cluster, ver o que
quebra na prática. Não tem mais débito estrutural que eu enxergo —
o que falta é uso.

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

- [x] Helm chart pra K8s (F3.3)
- [x] Agente K8s-native (`agent.engine=kubernetes`, F3.1+F3.2)
- [x] Secrets via K8s Secret refs (F3.4, `secrets.backend=kubernetes`)
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

## Plataforma — próximas ondas

### Já entregue (2026-04-23/24)

- ✅ **RBAC + audit log** — admin/maintainer/viewer, `/admin/users`,
  `/admin/audit`, `audit_events` com emissão em todo write. 6 commits
  (8158d59, 673acbd, 73e9992, 09c1b51, b2c595a, c13b82e).
- ✅ **Approval gates** — `approval:` no YAML, status `awaiting_approval`,
  allow-list por gate, UI Approve/Reject. 5 fases.
- ✅ **Cache step** — `cache:` no YAML + eviction (TTL + project + global
  quota) + purge UI.
- ✅ **Plugin system** — `uses: ...@v1`, `plugin.yaml` schema, catálogo
  in-memory, validação no apply, 6 plugins iniciais.
- ✅ **Pipeline services** — `services:` top-level com sidecars docker.
- ✅ **K8s engine + DinD sidecar** — `docker: true` em pipeline K8s.

### Próximas ondas (tamanho estimado)

**Small (≤ meio dia cada)**
- 💡 **SSE logs** — stream em vez de polling cursor-based.
- 💡 **Pipeline templates / `!include`** — snippet-sharing entre pipelines.
- 💡 **Docs site** — consolidar `docs/*.md` num site público + examples repo.
- 💡 **Test reports** — parser JUnit/Cobertura + aba Tests + flakiness
  history (design em `docs/test-reports-design.md` quando for a vez).

**Medium (1-2 dias cada)**
- 💡 **Notifications first-class** — `notifications:` top-level YAML
  (Slack/email/Discord) em vez de cada pipeline declarar um job.
- 💡 **PR builds end-to-end** — Checks API status + preview env +
  merge-gate.
- 💡 **Environments primitive** — `dev/staging/prod` como type + deploy
  history + rollback button.
- 💡 **SCM providers** — GitLab, Bitbucket, Gitea (cada um = adapter
  novo mas abstração `scm_sources` já suporta).
- 💡 **External secret managers** — cada provider = 1 adapter de
  `secrets.Resolver` (interface pronta desde commit `84092ca`).
  Projeto referencia o secret por nome; o resolver busca no
  provider configurado via env. Mascaramento automático no log
  (runner já recebe lista de valores pra substituir por `***`).
  Ordem sugerida por demanda imediata:
  - 💡 **HashiCorp Vault** (`VAULT_ADDR` + token/approle auth).
  - 💡 **AWS Secrets Manager** (SDK v2, IAM role ou static creds).
  - 💡 **GCP Secret Manager** (Application Default Credentials).
  - 💡 **Azure Key Vault** (last, menor base de usuários interna).

**Large (semana+)**
- 💡 **HA scheduler** — advisory lock Postgres OU etcd election.
- 💡 **Resource quotas por projeto/team** — multi-tenant real.
- 💡 **Pipeline deployment Argo-style** — `deployment:` primitive com
  helm/kustomize/manifests + desired/current state + rollback. Ver
  `roadmap_pipeline_deployment.md`.
- 💡 **ClickHouse / Loki pra logs em escala** — hoje `log_lines` em
  Postgres é OK pra dezenas de pipelines, vai apertar com centenas.
- 💡 **Chaos/resilience testing** — agent crash mid-job, DB failover,
  webhook duplicado sob carga.

## Plugin catalog

### Shipped (23)

- **build**: `node`, `go`, `maven`, `gradle`, `python`, `rust`.
- **container**: `docker`, `kaniko`, `buildx`, `docker-push`.
- **deploy**: `kubectl`, `helm`, `terraform`, `ansible`, `aws-cli`,
  `gcloud`.
- **security**: `trivy`, `gitleaks`, `cosign`.
- **release**: `github-release`.
- **notifications**: `slack`, `discord`, `email`.

### Próxima onda (ordem de prioridade)

Cada plugin = Dockerfile + entrypoint.sh + plugin.yaml. Template bem
estabelecido: tempo médio ~30min por plugin shell-thin (wrapper), ~2h
quando há lógica real (ex: `release-notes` auto-gen).

**Prioridade média**
- 💡 `gocdnext/teams`, `gocdnext/matrix` — notifications.
- 💡 `gocdnext/nexus-upload`, `gocdnext/artifactory`,
  `gocdnext/s3-upload`, `gocdnext/helm-push`.
- 💡 `gocdnext/tag`, `gocdnext/release-notes`.
- 💡 `gocdnext/codecov`, `gocdnext/coveralls`,
  `gocdnext/lighthouse-ci`.

**Prioridade baixa (chegam quando alguém pedir)**
- 💡 `gocdnext/dotnet`, `gocdnext/semgrep`, `gocdnext/snyk`,
  `gocdnext/sonarqube-scanner`.

## Parking lot (futuro, não priorizado)

- Marketplace externo de plugins / modelo de trust + signing.
- Config-as-code em repo separado (estilo GoCD config-repo).
- Single-job cancel por endpoint dedicado.
- Graceful SIGTERM→SIGKILL no cancel (hoje vai direto no SIGKILL).
- Per-attempt log history (job_run_attempt child table).
- Cascading rerun checks (needs_artifacts consumidos pela retention).
