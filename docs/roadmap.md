# Roadmap

## Phase 0 — Foundation (weeks 1–2)

- [x] Monorepo layout, go.work, go.mod per module
- [x] Proto contracts (agent, pipeline, common)
- [x] SQL schema + migrations
- [x] Domain types + YAML parser skeleton (with tests)
- [x] docker-compose dev stack (postgres + minio)
- [x] Dockerfiles (server, agent)
- [x] Makefile
- [x] Proto code generation wired (`make proto`)
- [ ] CI on gocdnext itself (GitHub Actions for now; dogfood later)

## Phase 1 — MVP pipeline (weeks 3–6)

- [x] GitHub webhook receiver + HMAC validation
- [x] Persist modifications from webhook payload
- [ ] Scheduler: NOTIFY loop, creates runs, dispatches jobs
- [ ] Agent: gRPC connect/register, clone git material, run `script:` inside image
- [ ] Agent: stream LogLine back to server
- [ ] Web UI: list pipelines + runs, live log view (SSE)
- [ ] `gocdnext validate` CLI working

**Exit criteria**: push a commit → pipeline runs → logs visible in UI.

## Phase 2 — The differentiator (weeks 7–10)

- [ ] Upstream material → fanout on pipeline success
- [ ] VSM endpoint + `@xyflow/react` visualization
- [ ] PR native support (branch-per-run, isolated counters)
- [ ] Auto-register webhook on GitHub (GitHub App flow)
- [ ] Rules evaluation (if / changes / when)
- [ ] Parallel matrix expansion
- [ ] Plugin step execution (Woodpecker contract)

**Exit criteria**: 1 pipeline with 2 downstream fanout, VSM renders, PR from a
fork triggers an isolated run.

## Phase 3 — Internal validation (weeks 11–14)

- [ ] Helm chart for K8s deployment
- [ ] Kubernetes-native agent (runs jobs as `Job`/`Pod`)
- [ ] Secrets via K8s Secret references
- [ ] Artifact upload to S3/MinIO
- [ ] Run 3–5 real pipelines from our own org
- [ ] Collect feedback, iterate UX

**Exit criteria**: gocdnext is running at least one of our *production-critical*
pipelines reliably for 2 weeks.

## Gate: open or stay internal?

Decision inputs:
- Did internal users actually prefer it over what they had?
- Is the VSM + fanout differentiator clearly valuable?
- Do we have bandwidth to maintain a public project?

## Parking lot (future)

- Multi-tenant + RBAC
- Plugin marketplace / trust model
- Approval gates (manual stage)
- Cron scheduling
- Config-as-code repo (pipelines from a separate config repo, GoCD-style)
- GitLab + Bitbucket webhook parity
- ClickHouse / Loki for logs at scale
- Distributed scheduler (multi-server HA)
