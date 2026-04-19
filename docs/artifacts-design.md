# Artifacts — Design Doc

Status: **draft, aguardando validação.** Nada foi codado ainda.

Objetivo: decidir como artefatos atravessam jobs/pipelines antes de escrever
código. O `examples/fanout/` de hoje é teatro: `build-core.build` declara
`artifacts: [bin/]` mas nada coleta, nada sobe, nada desce para
`deploy-api`/`deploy-worker`. Este doc fecha isso.

Não-objetivo desta fase: cache (decisão 6 explica por quê).

---

## Contexto já existente no repo

Peças que já estão no lugar e restringem as decisões:

- `docker-compose.yml` hoje sobe MinIO, mas a AGPL da MinIO Inc. fechou o
  caminho de uso embarcado / self-hosted sem licença comercial. **MinIO sai
  do docker-compose** como parte desta entrega (substituído por default
  filesystem local — ver Decisão 1).
- Parser YAML já aceita `artifacts: { paths, expire_in, when }` em
  [server/internal/parser/schema.go:83](server/internal/parser/schema.go#L83).
- Proto `ArtifactRef{path, url, size}` já existe e `JobResult` já carrega
  `repeated ArtifactRef artifacts = 6` em
  [proto/gocdnext/v1/agent.proto:77](proto/gocdnext/v1/agent.proto#L77).

Ou seja: o *contrato externo* (YAML + proto) já está quase pronto. O que falta
é a infra-estrutura (storage + upload/download) e a semântica do fanout.

---

## Decisão 1 — Storage backend

Com MinIO fora de cogitação (AGPL) e três backends reais já na mesa
(disco local para dev/airgap, S3 para AWS, GCS para GCP), o argumento de
YAGNI da v1 deste doc cai. Passa a valer o mesmo princípio que justificou
o `secrets.Resolver` no E1d: **múltiplas implementações reais = interface
desde já.**

**Opções:**

- **(A) S3-compat genérico via AWS SDK v2.** Um único client, endpoint
  configurável. Cobre AWS S3 e compatíveis (R2, Tigris, Backblaze, Ceph).
  Não cobre filesystem nem GCS nativo.
- **(B) Interface `artifacts.Store` com três backends concretos:**
  `filesystem`, `s3` (AWS SDK v2, endpoint-overridable pra R2/etc),
  `gcs` (google-cloud-go/storage).
- **(C) `artifacts.Store` + só 1 backend hoje (filesystem), S3/GCS
  ficam pra depois.**

**Recomendo (B).** Três backends concretos entregues juntos porque:

- **Filesystem** é o default de dev — zero dependências, zero licença
  hostil, survive laptop restart. Substitui o papel que o MinIO ocupava.
- **S3** cobre AWS + todos os S3-compat. Single client, endpoint
  configurável.
- **GCS** é nativo (não o shim S3-compat da Google) pra usar a auth
  padrão do GCP (Workload Identity, Application Default Credentials) —
  quem tá no GCP não quer mexer com access key.

A interface é pequena (~6 métodos) e estável (`Put`, `SignedPutURL`,
`SignedGetURL`, `Head`, `Delete`, `DeleteBatch`). Custo da abstração
real: uns 150 LOC de código + 3 impls; ganho: sem dor de cabeça de
licença, dev sem docker dependente, multi-cloud desde o dia 1.

Seleção por env var:

```
GOCDNEXT_ARTIFACTS_BACKEND=filesystem   # default
GOCDNEXT_ARTIFACTS_FS_ROOT=/var/lib/gocdnext/artifacts

GOCDNEXT_ARTIFACTS_BACKEND=s3
GOCDNEXT_ARTIFACTS_S3_BUCKET=...
GOCDNEXT_ARTIFACTS_S3_REGION=...
GOCDNEXT_ARTIFACTS_S3_ENDPOINT=...      # opcional; p/ R2, Tigris, etc.
GOCDNEXT_ARTIFACTS_S3_ACCESS_KEY=...    # ou IAM Role / IRSA
GOCDNEXT_ARTIFACTS_S3_SECRET_KEY=...

GOCDNEXT_ARTIFACTS_BACKEND=gcs
GOCDNEXT_ARTIFACTS_GCS_BUCKET=...
# credencial via ADC padrão (GOOGLE_APPLICATION_CREDENTIALS ou
# Workload Identity)
```

Server valida config no startup e cria o bucket/diretório se não existir.

---

## Decisão 2 — Protocolo de upload (agent → storage)

**Opções:**

- **(A) Agent recebe credencial do backend** e chama S3/GCS/filesystem
  direto.
- **(B) Agent faz upload por gRPC stream; server relaya para o storage.**
- **(C) Pre-signed URLs geradas pelo server; agent faz PUT HTTP com a URL
  assinada. Filesystem é servido via endpoint HTTP próprio do server com
  token assinado.**

**Recomendo (C).** Modelo uniforme pros três backends:

- **S3**: `PresignClient.PresignPutObject(...)` do AWS SDK v2 → URL
  `https://bucket.s3.region.amazonaws.com/key?X-Amz-Signature=...`.
- **GCS**: `bucket.SignedURL(key, opts)` do google-cloud-go → URL
  `https://storage.googleapis.com/bucket/key?X-Goog-Signature=...`.
- **Filesystem**: server expõe `PUT/GET /artifacts/{token}` onde
  `token` é um JWT/PASETO curto assinado com uma chave interna do
  server, codificando `storage_key + verb + expires_at`. Handler escreve
  em `GOCDNEXT_ARTIFACTS_FS_ROOT/{storage_key}`. O agent **não sabe** qual
  backend está atrás — sempre vê "URL + verbo HTTP".

Vantagens de manter URL assinada também no filesystem:

- Contrato único pro agent: um campo `put_url` / `get_url`, mesmo fluxo.
- O token no filesystem substitui o cookie S3/GCS, mantendo o princípio
  "agent nunca vê credencial persistente."
- Função `Store.SignedPutURL(key, ttl)` / `SignedGetURL(key, ttl)` é o
  único ponto de variação da interface.

Protocolo concreto: novo RPC `AgentService.RequestArtifactUpload(run_id,
job_id, paths[]) → repeated {path, put_url, expires_at}`. Agent tara+gz
por path, `PUT` com `Content-Length`. No fim, envia `JobResult` com
`ArtifactRef{path, url: <backend-opaco>, size, sha256}`. Server confirma
via `Store.Head(key)` antes de marcar o job succeeded.

Alternativa descartada (B): tráfego dobrado pelo server, complicação do
bidi stream atual, sem ganho.

---

## Decisão 3 — Retenção / limpeza

TTL isolado não basta. Projeto abandonado com `expire_in: 365d` vira lixo
eterno; pipeline que roda de hora em hora enche bucket mesmo com 30d.
Precisa de **várias dimensões**, aplicadas em camadas da mais forte pra
mais fraca:

### Camadas de limpeza

**Camada 1 — TTL explícito (hard expire).** Default global
`GOCDNEXT_ARTIFACT_DEFAULT_TTL=30d`, YAML sobrescreve por job
(`expire_in: 7d`). Formato: `1h`, `7d`, `30d`, `0` = pinned.
Row nasce com `expires_at = now() + ttl`. Sweeper deleta quando
`expires_at < now()`.

**Camada 2 — Count-based keep-last por pipeline.** Manter os últimos **N
runs com artefato** por pipeline. Default `GOCDNEXT_ARTIFACT_KEEP_LAST=30`.
YAML pode sobrescrever a nível de pipeline (`retention.keep_last: 50`).
Quando a 31ª run entra, artefatos da mais antiga são marcados expirados
mesmo que o TTL ainda não tenha estourado. **Isso é o que os usuários
reais de GoCD/Jenkins esperam** ("discard old builds"); TTL sozinho dá
lixo em projeto abandonado.

**Camada 3 — Quota por projeto (soft cap).**
`GOCDNEXT_ARTIFACT_PROJECT_QUOTA_BYTES=100GB` (default, configurável
por projeto via UI/API futuramente). Quando projeto passa de 80% → warn
no UI. Passa de 100% → próximo upload falha com
`ARTIFACT_QUOTA_EXCEEDED`. Sweeper LRU dentro do projeto (artefato mais
velho não-pinned primeiro) libera espaço no tick seguinte.

**Camada 4 — Quota global (hard cap).**
`GOCDNEXT_ARTIFACT_GLOBAL_QUOTA_BYTES` (opcional, default desligado).
Último freio contra lotar disco/bucket. Upload falha quando estourado.

**Pin / override.** `expire_in: 0` ou, via UI/API, "pin this run."
Marca `pinned_at`; sweeper ignora em TODAS as camadas — release de
produção não some por descuido.

**`when: always|on_success|on_failure`** do YAML vale pra *se* sobe o
artefato, não pra retenção. (Rodou e falhou → `on_failure` sobe o
coredump; a retenção depois segue as 4 camadas normalmente.)

### Ordem aplicada pelo sweeper

```
tick a cada 10 min:
  1. DELETE onde expires_at < now()                  (TTL duro)
  2. Por pipeline: marcar expired as runs > keep_last (count cap)
  3. Por projeto acima de quota: LRU até ficar dentro (soft cap)
  4. Global acima de quota: LRU até ficar dentro      (hard cap)
  5. Para cada batch: delete no storage, depois delete no DB
```

### Schema extra pra suportar as camadas

Adiciono à tabela `artifacts` da Decisão 4:

```sql
ALTER TABLE artifacts
  ADD COLUMN pipeline_id UUID NOT NULL REFERENCES pipelines(id)
    ON DELETE CASCADE,       -- camada 2 precisa agrupar por pipeline
  ADD COLUMN project_id  UUID NOT NULL REFERENCES projects(id)
    ON DELETE CASCADE,       -- camada 3 precisa agrupar por projeto
  ADD COLUMN pinned_at   TIMESTAMPTZ,    -- NULL = não pinado
  ADD COLUMN deleted_at  TIMESTAMPTZ;    -- marca antes de remover, pra
                                         --   idempotência do sweeper

CREATE INDEX ON artifacts (project_id, created_at)
  WHERE deleted_at IS NULL AND pinned_at IS NULL;
CREATE INDEX ON artifacts (pipeline_id, created_at)
  WHERE deleted_at IS NULL AND pinned_at IS NULL;
```

(Na prática jogo esses campos direto no `CREATE TABLE` da D4 ao invés
de um `ALTER` — só listei separado aqui pra isolar o que a retenção
cobra.)

### Detalhes operacionais

- **Idempotência do sweeper.** Marca `deleted_at = now()` no DB antes de
  chamar `Store.Delete(key)`. Se o processo morre no meio, o próximo
  tick pega linhas com `deleted_at NOT NULL AND older_than(5min)` e
  re-tenta. `Store.Delete` de key inexistente é no-op em todos os 3
  backends.
- **Batch size.** Limite 500 deleções por tick. S3/GCS `DeleteObjects`
  suportam 1000/request — passo em 2 páginas. Filesystem é `os.Remove`
  sequencial; 500 em ~100ms local.
- **Reconciliação storage↔DB.** Cron diário lista `storage_key`
  retornados pelo backend e compara com DB. Key que não está em
  `artifacts` → órfão; delete. Defesa em profundidade contra crash entre
  `Store.Delete` e `DELETE FROM artifacts`.
- **Métricas.**
  - `gocdnext_artifact_storage_bytes{backend, project}` (gauge, atualizado
    a cada tick)
  - `gocdnext_artifact_sweeper_deleted_total{reason}` (counter; `reason` ∈
    `ttl|keep_last|project_quota|global_quota|orphan`)
  - `gocdnext_artifact_sweeper_bytes_freed_total`
  - `gocdnext_artifact_sweeper_errors_total{backend}`
- **Quota UI.** Página do projeto mostra: "47 GB de 100 GB usados (47%).
  Artefatos mais antigos serão removidos automaticamente." Com uma lista
  top-10 "pipelines consumindo mais espaço" pra dar acionabilidade ao
  usuário.

### Defaults resumidos

| Dimensão              | Default                     | Override             |
|-----------------------|-----------------------------|----------------------|
| TTL                   | 30 dias                     | YAML `expire_in:`    |
| Keep-last por pipeline| 30 runs                     | YAML `retention.keep_last:` |
| Quota por projeto     | 100 GB                      | Config por projeto   |
| Quota global          | desligada                   | env var              |
| Pin                   | desligado                   | YAML `expire_in: 0` ou UI |
| Tick do sweeper       | 10 min                      | `GOCDNEXT_ARTIFACT_SWEEP_INTERVAL` |

Estes números são chutes razoáveis; ajustáveis no primeiro uso real.

---

## Decisão 4 — Naming + scoping de keys no storage

**Opções:**

- **(A) Path-based:** `artifacts/{project_id}/{pipeline}/{run_number}/{job_name}/{attempt}/{path}`.
  Humano debugável via `mc ls`.
- **(B) UUID-based:** `artifacts/{artifact_id}`. Compacto, agnóstico,
  requer metadata no DB para saber o que é.
- **(C) Híbrido:** key no storage é UUID, tabela `artifacts` guarda
  metadados (path original, job, run, attempt, content_type, size,
  checksum, expires_at).

**Recomendo (C).** Path-based vaza convenção pra dentro do storage (renomeei
pipeline? oras, o path velho fica órfão). UUID na storage + metadados no
Postgres é a divisão certa: storage é burro (key+bytes), DB é autoridade
(quem, quando, por quê, expira quando).

Schema (nova migration `00005_artifacts.sql`):

```sql
CREATE TABLE artifacts (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  run_id         UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  job_run_id     UUID NOT NULL REFERENCES job_runs(id) ON DELETE CASCADE,
  path           TEXT NOT NULL,         -- path relativo declarado no YAML
  storage_key    TEXT NOT NULL UNIQUE,  -- chave opaca no bucket
  size_bytes     BIGINT NOT NULL,
  content_sha256 TEXT NOT NULL,         -- agent computa e manda
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at     TIMESTAMPTZ            -- NULL = pinned
);
CREATE INDEX ON artifacts (run_id);
CREATE INDEX ON artifacts (expires_at) WHERE expires_at IS NOT NULL;
```

`content_sha256` é grátis (agent já lê os bytes pra tarar) e dá dedupe
futuro + verificação de integridade no download.

---

## Decisão 5 — Escopo de download (quem enxerga o quê)

Esta é a decisão que **o fanout cobra.** Três escopos possíveis, do mais
simples pro mais útil:

- **(A) Intra-job apenas.** Artefato existe só pra ser lido de volta num
  retry do mesmo job. Inútil pro fanout, descartar.
- **(B) Intra-run: outros jobs da mesma run (stage ↑) enxergam.** Cobre
  `build → test → deploy` dentro de *um* pipeline.
- **(C) Intra-run + fanout downstream:** pipeline downstream disparado por
  `upstream: { pipeline: build-core, stage: test }` enxerga os artefatos
  da run upstream que o disparou. **É o diferencial herdado do GoCD** —
  a run upstream tem uma identidade estável (`run_id`), as runs downstream
  são "filhas" dela, compartilham a mesma revisão de material e puxam
  artefatos dela.

**Recomendo (B) + (C) desde o início.** (B) é implementação trivial
(resolver `run_id` do job → query artifacts); (C) custa só o vínculo
`downstream_run.upstream_run_id` que o scheduler já está gravando
implicitamente no `revisions` JSONB da run. Formaliza e expõe.

YAML para consumo downstream (novo, mínimo):

```yaml
# deploy-api.yaml
jobs:
  deploy:
    needs_artifacts:
      - from: build-core         # nome do pipeline upstream
        job: build               # nome do job
        paths: [bin/core]        # subset opcional; default = tudo
        dest: ./bin              # onde baixar no workspace
```

Scheduler resolve ao montar `JobAssignment`: consulta a run upstream que
triggou a run atual, puxa metadados dos artefatos matching e emite
pre-signed **GET** URLs (TTL 15 min) no novo campo
`JobAssignment.artifact_downloads = [{storage_key, get_url, dest_path,
sha256}]`. Agent baixa + descompacta antes da 1ª task.

Intra-run (B) usa a mesma mecânica mas sem precisar do `from:` — default
implícito: se `needs: [build]` e `build` tem artefatos, todos eles descem.
(A decidir: automático por `needs:` ou explícito sempre? **Explícito** —
menos mágica, vai bem com o estilo declarativo do resto do YAML.)

---

## Decisão 6 — Relação com `cache:`

**Opções:**

- **(A) Cache é sinônimo de artefato com retenção curta + key
  determinística** (`key: ${lockfile.sha256}-${GOARCH}`). Mesma tabela,
  mesmo storage, flag `is_cache: bool`.
- **(B) Cache é sistema separado** (bucket próprio, sweeper próprio,
  key-value em vez de path-based, LRU em vez de TTL).
- **(C) Não fazer cache agora.** Escopo C5/C6 é só artefato. Cache vira
  slice própria depois.

**Recomendo (C).** Semânticas diferentes encostadas cedo demais geram
abstração errada. Cache é:

- key-addressed (hash), não path-addressed;
- mutável (mesmo key, upload novo sobrescreve);
- LRU, não TTL;
- otimização (não quebra o build se falhar o download).

Artefato é:

- path-addressed (`bin/core`);
- imutável;
- TTL;
- contrato (se falhar o download, o downstream não roda).

Juntar vira peso morto. Fazer cache num slice próprio depois —
provavelmente D-alguma-coisa — quando o primeiro pipeline real começar a
sentir build lento. Hoje, `Cache` no schema fica como campo parseado mas
ignorado (warning no validator: "cache: not yet implemented").

---

## Test reports (não-objetivo v1)

Reports de teste (JUnit XML, Cobertura, LCOV) **não** são tratados como
categoria própria nesta fase. Motivo: o upload genérico já cobre o passo
"preservar o arquivo"; a parte de valor (parsear, popular contadores,
aba Tests no UI, histórico de flakiness) é uma feature própria que não
bloqueia dogfood.

O que funciona **imediatamente** depois do E2a (grátis):

```yaml
jobs:
  test:
    script:
      - go test -v 2>&1 | go-junit-report > report.xml
    artifacts:
      paths: [report.xml, coverage.out]
      when: always   # sobe mesmo com teste vermelho
```

O que fica pra **E3 — Test Reports**:

1. Extensão de schema YAML: `artifacts.reports: { junit: [paths],
   coverage: [paths] }` — marca artefato com tipo conhecido.
2. Parser server-side (JUnit XML primeiro; `encoding/xml` resolve).
3. Tabela `test_results(run_id, job_id, suite, test, status, duration,
   failure_message, stdout)`.
4. Aba "Tests" na página do run, drill-down, badges de cobertura,
   histórico de flakiness por teste.

Design doc próprio quando for a vez (`docs/test-reports-design.md`).

---

## Fluxo end-to-end (upload → fanout download)

Pra fechar visualmente:

1. `build-core.build` roda, gera `bin/core`.
2. Agent pede `RequestArtifactUpload(run_id, job_id, ["bin/"])`.
3. Server insere linhas em `artifacts` (status `pending`), gera pre-signed
   PUT URLs, responde.
4. Agent tara `bin/`, faz PUT, responde `JobResult` com `ArtifactRef`
   incluindo `sha256`.
5. Server faz HEAD no storage, confirma, marca `artifacts.status =
   'ready'`.
6. `build-core.test` passa → scheduler dispara `deploy-api` e
   `deploy-worker` (fanout paralelo, mesma revisão).
7. Ao montar `JobAssignment` do `deploy-api.deploy`, scheduler resolve
   `needs_artifacts.from: build-core`, encontra a run upstream,
   emite pre-signed GETs.
8. Agent baixa + verifica sha256 + descompacta em `./bin/`, roda as tasks.

Falhas possíveis e tratamento explícito:

- Upload falhou → job vai `failed` com erro "artifact upload failed";
  `artifacts.status=pending` sobre no sweeper próprio após 1h.
- Download upstream não existe → job `failed` com "required artifact X
  from Y/Z not found" antes de rodar tasks. **Falha rápida, não queima
  minuto de build.**
- Sha mismatch → job falha com "artifact checksum mismatch" (corrupção
  storage / MitM se TLS off).

---

## Plano de implementação (se aprovado)

Divide em 4 commits limpos no `main`:

1. **E2a — Store interface + filesystem backend + upload.**
   `artifacts.Store` interface, impl filesystem + handler HTTP
   `/artifacts/{token}` com PASETO assinado, `artifacts` table, proto
   mudanças, `RequestArtifactUpload` RPC, agent-side upload. Remove
   MinIO do docker-compose. Teste E2E: 1 job declara `artifacts:
   [bin/]`, verificar row no DB + arquivo em `$FS_ROOT` + sha256 match.
2. **E2b — S3 backend + GCS backend.** Duas impls concretas da mesma
   interface. Testes: `testcontainers-go` com LocalStack para S3;
   `fake-gcs-server` para GCS. Nenhuma mudança no agent (mesma URL
   assinada).
3. **E2c — intra-run download.** `needs_artifacts` no parser, scheduler
   resolve intra-run, `JobAssignment.artifact_downloads`, agent-side
   download + unpack. Teste E2E: 2 jobs sequenciais, primeiro gera,
   segundo consome (rodado nos 3 backends via matrix no CI).
4. **E2d — fanout download + sweeper de retenção.** Vínculo
   `upstream_run_id` explícito, resolver cross-run, sweeper. Teste E2E:
   `examples/fanout/` fica **real** — `build-core.build` gera
   `bin/core`, `deploy-api` e `deploy-worker` baixam em paralelo.

Depois desses 4, a dogfood-readiness volta pra mesa: artefato é o último
bloqueador estrutural que eu enxergo pra gente rodar uma pipeline real.

---

## Pontos a validar com Kleber

Reler as 6 decisões, marcar uma das três opções:

- **"concordo"** → sigo implementação no plano acima.
- **"troca X para opção Y"** → eu atualizo o doc, você revalida.
- **"isso aqui é besteira, pensa de novo"** → reescrevo.

Pontos em que tenho menos convicção e onde um redirecionamento teu é
especialmente bem-vindo:

- Decisão 1: entrego os 3 backends de uma vez (E2a filesystem + E2b
  S3/GCS) ou só filesystem agora e S3/GCS ficam por demanda? Entregar
  os 3 junto força a interface a ficar certa; entregar só 1 corre risco
  da interface vazar detalhe de filesystem.
- Decisão 2: filesystem via endpoint HTTP do server com token assinado
  **passa tráfego de upload/download pelo server**. Pra dev tá ok; pra
  prod quem escolheu filesystem provavelmente é deploy small/single-node
  onde isso não dói. Mas vale confirmar que essa assimetria (S3/GCS
  bypassam o server, filesystem não) é aceitável.
- Decisão 5 opção (C) fanout. Ambicioso; posso cortar pra só (B) em E2c
  e adiar fanout pra E2d como planejado OU adiar pra uma fase E3.
- Decisão 6 punt do cache. Se já tem pipeline real esperando cache (ex:
  Go monorepo com `go build` lento), vale antecipar.
