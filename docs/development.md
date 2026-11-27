# Development

This is a task guide to building, testing and releasing the operator. The
pipeline itself is documented in [`.github/WORKFLOWS.md`](../.github/WORKFLOWS.md).

## Build and Test

Tasks are in [`taskfile.yml`](../taskfile.yml).

```bash
task build        # bin/kiwix-operator
task test         # go test -race
task lint         # static checks only
task ci           # everything CI runs; run before pushing
task test:kiwix   # kiwix-tagged tests against real kiwix-serve, in docker (slow)
task e2e          # kind end-to-end (needs docker, kind and helm; up to 45 minutes)
task generate     # DeepCopy
task manifests    # CRDs, ClusterRole, and the chart's copy of the CRDs
```

`task verify-generated` fails when generated artifacts (deepcopy, CRDs, RBAC,
chart copies) are stale, so run `task generate manifests` after changing the
API types or kubebuilder markers.

| Level | What it covers |
|---|---|
| Unit | Health gate, pre-checks, schedule and backoff, catalog parsing, name rules, manifest recovery, attempt states, controllers on a fake client |
| envtest | CEL validation, defaults and immutability of the two CRDs |
| `task test:kiwix` | The librarian against a real kiwix-serve: zero-book start, rename-over swaps, confirmed reloads, crash recovery, the lock |
| `task e2e` | A kind cluster: a crawl of an in-cluster site under the locked-down settings, a fake catalog, a webhook sink, and a crawl denied egress without the allow flag |

`task ci` enforces a 70% coverage floor over `./internal/...`.

The e2e builds the image, creates a throwaway kind cluster with its own
kubeconfig (`/tmp/kiwix-e2e.kubeconfig`), installs the chart, and deletes the
cluster afterwards. It never touches your default kube context. The `ci` check
does not run it, so run it before a release or a change to the chart, the
controllers or the librarian.

## CI

`ci.yaml` runs on `push` only: tests on every branch, then a multi-arch image
build and smoke test (`ghcr.io/jacaudi/kiwix-operator:sha-<short>`) for an open
PR's branch, `main` or a manual dispatch. The one required check is `ci`.

**A pull request from a fork gets no `ci` check**, because the workflow has no
`pull_request` trigger. A maintainer re-pushes the fork's branch to this
repository, which runs `ci` and reports it on the PR, or merges as admin. Adding
a `pull_request` trigger with a job-level `if` is rejected: the skipped
duplicate `ci` counts as passing.

## Releases

release-please reads Conventional Commits on `main` and keeps a release PR
open. Merging it tags `vX.Y.Z`, publishes the multi-arch image and publishes
the chart to `oci://ghcr.io/jacaudi/charts/kiwix-operator` in lockstep, with the
chart version equal to the tag.

| Commit type | Bump |
|---|---|
| `fix`, `perf`, `refactor`, `deps`, `revert` | patch |
| `feat` | minor |
| `feat!` or `BREAKING CHANGE` | major, even while 0.x |
| `docs`, `chore`, `style`, `test`, `build`, `ci` | none |

[← Back to README](../README.md)
