# Plugin base image mirrors

Some plugin base images are mirrored into GHCR instead of pulled from upstream
at build time. The reason is always the same: the upstream registry rate-limits
**anonymous** pulls, and the Plugins CI matrix pulls many bases concurrently
from one runner IP, so a release build flakes with `toomanyrequests: Data limit
exceeded`. GHCR — the registry the plugins already push to and authenticate
against in CI — has no such anonymous cap.

## Mirrors

| GHCR mirror | Upstream | Used by |
|---|---|---|
| `ghcr.io/klinux/gocdnext-base-aws-cli:2.22.8` | `public.ecr.aws/aws-cli/aws-cli:2.22.8` | `aws-cli`, `s3` |

The mirror is **private but linked to this repo** (via the
`org.opencontainers.image.source` label), so the Plugins workflow's
`GITHUB_TOKEN` (`packages: read`) pulls it without the package being public.

## Re-mirroring (on a base version bump)

Bump the base version in the plugin `Dockerfile`(s), then publish the new tag.
Build it as a labeled image (NOT `imagetools create`, which copies the upstream
manifest verbatim and would drop the repo-link label, breaking CI pulls):

```sh
# token needs write:packages on the klinux packages
echo "$GHCR_TOKEN" | docker login ghcr.io -u <user> --password-stdin

printf 'FROM public.ecr.aws/aws-cli/aws-cli:<new>\nLABEL org.opencontainers.image.source=https://github.com/klinux/gocdnext\n' \
  | docker buildx build --platform linux/amd64 -t ghcr.io/klinux/gocdnext-base-aws-cli:<new> --push -f - .
```

Plugins build `linux/amd64` only today, so an amd64 mirror is enough; add arm64
to the `--platform` list if/when the Plugins matrix builds multi-arch.
