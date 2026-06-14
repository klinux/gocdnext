---
title: Plugin versioning & pinning
description: "What image_tag means, what pinning to @v1 actually guarantees, and the policy plugin authors follow when shipping a breaking change."
---

Every plugin you reference with `uses: gocdnext/<name>@<channel>` resolves
to a container image tag. This page explains what the channels mean for
**users pinning a plugin**, and the policy **plugin authors** follow when
a change is breaking.

## The channel model

Each plugin declares a contract channel in its
[`plugins/<name>/plugin.yaml`](https://github.com/klinux/gocdnext/tree/main/plugins):

```yaml
name: node
image_tag: v2   # the channel `uses: gocdnext/node@v2` resolves to
```

`image_tag` (default `v1`) is the **single source of truth** for the
channel — the catalog page renders `uses:` examples against it, and the
publish workflow tags the image with it.

On a **release-tag push** (`vX.Y.Z`) the workflow publishes each plugin
image under three stable tags pointing at the **same digest**:

- `latest`
- `v1`
- the manifest's `image_tag` (e.g. `v2` for node)

Non-release pushes (a merge to `main`, a PR) publish only **tracker**
tags (`main`, `pr-N-sha-…`) — never the stable channels. So pinning to
`@v1` or `@latest` tracks the **last release**, not every commit on main.

### What `@v1` actually guarantees

This is the part that bites: **`v1` is always republished alongside the
current `image_tag`, at the same digest.** It is *not* a frozen "major
version 1" channel.

Concretely: `gocdnext/node` declares `image_tag: v2`, but `node:v1` has
carried the v2 contract since the v0.4.39 rewrite — the two tags are the
same image. A pipeline pinned to `gocdnext/node@v1` silently moved to v2
behaviour.

So today there are two real guarantees, and one common misconception:

- ✅ `@<image_tag>` (e.g. `@v2`) — the plugin's current advertised
  contract. Stable across non-release commits.
- ✅ `@latest` — same as above, the latest released contract.
- ❌ `@v1` is **not** "frozen at major 1." It tracks current content. Do
  not rely on it to shield a pipeline from a breaking change.

There is no frozen-old-major channel today. A pipeline that must not move
across a breaking plugin change should pin a **release tag** of gocdnext
itself (which pins the whole catalog at that point), not the plugin's
`@v1`.

## Authoring policy: when to bump `image_tag`

**Bump the major** (e.g. `v1` → `v2`) when a change breaks the plugin's
public contract:

- a `with:` input is removed or renamed,
- a default changes in a way that alters existing pipelines' behaviour,
- output aliases (`$GOCDNEXT_OUTPUT_FILE` keys) are removed or renamed,
- the command/entrypoint semantics change incompatibly.

**Do not bump** for backward-compatible changes — a new *optional* input,
a bug fix, a base-image security patch, added outputs. Those ride the
existing channel.

## Shipping a breaking change

When you bump `image_tag`, do all three so users aren't surprised:

1. **Bump `image_tag` in `plugin.yaml`.** This is what flips the catalog
   page's `uses:` examples and the published channel.
2. **Document the break in the plugin's `README.md`** — a "Breaking
   change from vN" note plus a migration section. See
   [`plugins/node/README.md`](https://github.com/klinux/gocdnext/blob/main/plugins/node/README.md)
   for the reference shape.
3. **Add a CHANGELOG entry** under the release that ships it, calling out
   the plugin, the break, and the migration.

Because `@v1` keeps tracking the new content (see above), a breaking bump
*will* reach pinned-to-`v1` users on the next release — the README +
CHANGELOG are how they find out what changed. Treat them as the contract,
not a courtesy.

## See also

- [Plugin catalog](/gocdnext/docs/reference/plugins/) — every plugin and
  its current `image_tag`
- [YAML reference: plugin jobs](/gocdnext/docs/pipelines/yaml-reference/)
