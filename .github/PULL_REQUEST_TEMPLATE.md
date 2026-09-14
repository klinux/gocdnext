<!--
Thanks for contributing to gocdnext! Keep PRs small and focused: one PR = one
feature/fix. A large refactor should live in its own PR, separate from a feature.
-->

## What & why

<!-- What does this change do, and why? Link the issue it closes. -->

Closes #

## How it was tested

<!-- gocdnext is TDD-first. Point at the new/changed test that covers this path. -->

- [ ] New or updated test covering the changed path (red → green)
- [ ] `make test` passes locally **with the race detector**
- [ ] `make lint` clean

## Checklist

- [ ] Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/) (`feat(scope):`, `fix(scope):`, `docs:`, `chore:`, `refactor:`, `test:`).
- [ ] No source file over ~400 lines (test files may exceed when cohesive).
- [ ] Proto changed? Ran `make proto`; `buf lint` / `buf breaking` clean (generated code not hand-edited).
- [ ] DB changed? Added a **forward-only** goose migration; regenerated sqlc; no `.down.sql` meant for production.
- [ ] Parser/schema changed? Ran `make schema` so the committed JSON Schema is not stale.
- [ ] User input reaches shell/SQL/exec/log? It is validated/sanitised; secret values are added to `LogMasks` in the same step they're injected.
- [ ] Docs updated for user-facing changes (`docs/src/content/docs/**`).
- [ ] No customer/company references or real credentials in the diff.

## Notes for reviewers

<!-- Anything worth calling out: perf numbers, benchmark alloc diffs, EXPLAIN ANALYZE for new queries, screenshots for UI. -->
