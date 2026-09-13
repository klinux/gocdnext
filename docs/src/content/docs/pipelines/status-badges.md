---
title: Status badges
description: Publish gocdnext build status as a README/wiki SVG badge.
---

gocdnext can serve shields-style SVG badges for a project or a single pipeline:

```text
GET /api/v1/badge/<project>.svg?token=<badge-token>
GET /api/v1/badge/<project>/<pipeline>.svg?token=<badge-token>
```

The badge shows the latest matching run as:

- `passing` for a successful run
- `failing` for a failed or canceled run
- `running` for queued, waiting, or running work
- `unknown` when there is no matching run

Badges are anonymous so GitHub, docs sites, and dashboards can embed them without a session cookie. When the project has an SCM source, a badge without `branch` uses that source's default branch. Add `branch=<name>` to override it.

## Security model

Badges are disabled by default. Enabling a badge creates an unguessable project token and stores only its SHA-256 hash. A request with a missing, malformed, disabled, unknown, or wrong token returns the same grey `unknown` SVG with HTTP 200. This avoids turning the public endpoint into a project-existence or run-history oracle.

Treat the token like a publish link. Anyone who has it can see the coarse status of that project or pipeline. Rotate it to invalidate existing badge URLs, or disable badges for the project.

## Enable or rotate

Use a maintainer or admin session/API token:

```bash
curl -X POST \
  -H "Authorization: Bearer $GOCDNEXT_TOKEN" \
  https://ci.example.com/api/v1/projects/payments/badge/token
```

The response returns the plaintext token once, plus a ready-to-copy Markdown snippet:

```json
{
  "enabled": true,
  "token": "43-character-url-token",
  "badge_url": "https://ci.example.com/api/v1/badge/payments.svg?token=43-character-url-token",
  "markdown": "![build](https://ci.example.com/api/v1/badge/payments.svg?token=43-character-url-token)"
}
```

Pin the badge to a non-default branch by appending `&branch=<name>`.

For a specific pipeline, insert the pipeline name before `.svg`:

```markdown
![build](https://ci.example.com/api/v1/badge/payments/api-test.svg?token=43-character-url-token&branch=main)
```

## Disable

```bash
curl -X DELETE \
  -H "Authorization: Bearer $GOCDNEXT_TOKEN" \
  https://ci.example.com/api/v1/projects/payments/badge/token
```

After disable, old badge URLs continue to render a valid `unknown` SVG.
