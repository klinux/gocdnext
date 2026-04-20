# gocdnext Helm chart

Deploys **server + agent + web** to Kubernetes.

Postgres is intentionally NOT bundled for production — point at a
managed DB via `database.url` or `database.existingSecret`. A single-
replica dev StatefulSet is available via `devDatabase.enabled` for
smoke testing the chart only.

## Quick start (dev)

```
helm install gocdnext ./charts/gocdnext \
  --namespace gocdnext --create-namespace \
  --set devDatabase.enabled=true \
  --set webhookToken.value=$(openssl rand -hex 16) \
  --set secretKey.value=$(openssl rand -hex 32) \
  --set artifactsSignKey.value=$(openssl rand -hex 32) \
  --set agent.tokenSecret.value=$(openssl rand -hex 16)
```

Port-forward for local access:

```
kubectl -n gocdnext port-forward svc/gocdnext-gocdnext-server 8153
kubectl -n gocdnext port-forward svc/gocdnext-gocdnext-web 3000
```

Then seed an agent row so the agent can register (same token you
passed to Helm):

```
kubectl -n gocdnext exec -it statefulset/gocdnext-gocdnext-postgres -- \
  psql -U gocdnext -c "INSERT INTO agents (name, token_hash) VALUES ('dev', encode(sha256('<token>'::bytea), 'hex'));"
```

## Prod checklist

- Point `database.url` at a managed Postgres (e.g. RDS, Cloud SQL).
- Set `server.publicBase` to the externally-reachable URL. Webhook
  auto-register and Checks API both depend on it.
- Enable `server.ingress.enabled=true` and terminate TLS at the
  ingress (or above). Don't expose ClusterIP directly.
- Use `existingSecret` for every credential (`webhookToken`,
  `secretKey`, `artifactsSignKey`, `agent.tokenSecret`, GitHub App,
  artifact backend). Inline values are for dev smoke only.
- Pick the artifact backend that matches your infra:
  - `filesystem` — PVC on the server Pod; simplest.
  - `s3` — AWS S3 or compatible (R2, Tigris). IRSA is supported:
    leave `s3.existingSecret` empty to use the Pod's IAM role.
  - `gcs` — requires a service-account JSON in a Secret.
- Size the agent workspace PVC for your largest artefact + checkout.
  `ReadWriteMany` lets agent + job Pods spread across nodes.

## Agent engine

The agent runs scripts inside one of two runtimes, chosen at boot:

| engine       | behaviour                                |
|--------------|------------------------------------------|
| `shell`      | `sh -c $script` on the agent host        |
| `kubernetes` | Each task runs as a fresh Pod in the namespace, mounting the shared workspace PVC |

Chart default is `kubernetes`. The Role this chart creates grants
the agent `pods` + `pods/log` CRUD scoped to the release namespace
— nothing beyond. Override with `--set agent.engine=shell` if
you're running the agent as a VM/bare-metal binary and want to keep
tasks on the same host.

## GitHub App

Enable to get auto-register webhook + Checks API:

```
--set githubApp.enabled=true \
--set githubApp.appID=12345 \
--set githubApp.existingSecret=github-app   # Secret with key `private-key`
```

The server auto-mounts the key at `/var/run/secrets/gocdnext/github-app-private-key`.
