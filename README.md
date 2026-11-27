# kiwix-operator

A Kubernetes operator that runs a self-hosted [Kiwix](https://kiwix.org)
library and keeps its ZIM files fresh. You declare each archive, either a site
to crawl with [zimit](https://github.com/openzim/zimit) or a book to download
from the Kiwix catalog or a URL, and the operator fetches it, checks it, swaps
it in and tells you.

It is not a Kiwix client or a general backup tool. It manages ZIM files in one
volume per library, and nothing else mounts that volume.

> [!WARNING]
> **Agentically generated.** This codebase was produced through agentic,
> spec-driven development: each feature began as a written design and
> implementation spec, then a coding agent executed the plan under human
> review. Tests, code review, and CI gates apply as they would for any
> project, but the authorship pattern is not a single human contributor —
> keep that in mind when evaluating fit for your environment.
>
> This is an independent project, not an official Kiwix or openZIM operator.

## What It Does

Create a `KiwixLibrary` and the operator deploys kiwix-serve with a librarian
sidecar that owns the library volume. Create `ZimArchive` objects in the same
namespace and each one is crawled or downloaded, on a cron schedule or on
demand. A new version replaces the old one only if it passes a health check,
kiwix-serve picks it up with no restart, and each archive keeps the same
`/content/<archive>` URL. Failures and rejected refreshes go to a webhook.

## Features

- **Crawl sites** — zimit runs as a Job on any node, in a locked-down pod, and
  uploads the finished ZIM to the library over HTTP.
- **Download books** — by name from the Kiwix catalog (a newer version is
  fetched when one appears) or from a fixed URL with an optional SHA-256.
- **Health-gated refresh** — a takedown page or truncated crawl never replaces
  the last good ZIM; the old file stays and you are notified.
- **Live reload** — `library.xml` is rewritten and confirmed by book id, so
  kiwix-serve needs no pod restart.
- **Adoption** — `adoptFile` takes over a ZIM already on the volume, so an
  existing library migrates without a re-download.
- **Webhook notifications** — `{"title","message"}` JSON for failed, rejected
  and, per archive, succeeded attempts.
- **Explicit deletion** — `deletionPolicy: Retain` or `Delete` decides what
  happens to the file; removing an object never deletes data on its own.
- **HTTPRoute** *(optional)* — set `spec.route` to publish the library through
  the Gateway API.
- **Kept WARCs** *(optional)* — `keepWarcs` stores the raw crawl so a ZIM can
  be rebuilt after the site is gone.

## Quick Start

Prerequisites: Kubernetes 1.31 or newer (the chart's `kubeVersion`), Helm with
OCI support, and a default StorageClass for the library volume. For `spec.route`
you also need the Gateway API CRDs. NetworkPolicies are created for every
library; they only take effect on a CNI that enforces them.

### Install the Operator

Pick a version from the
[releases](https://github.com/jacaudi/kiwix-operator/releases) page. The chart
installs both CRDs and the controller.

```bash
helm install kiwix-operator oci://ghcr.io/jacaudi/charts/kiwix-operator \
  --version <version> --namespace kiwix-operator --create-namespace
```

Do not set `global.nameOverride`. The library's NetworkPolicy admits the
operator by its `app.kubernetes.io/name: kiwix-operator` pod label, and an
override changes that label, which cuts the manager off from every librarian.

### Create a Library and an Archive

```yaml
apiVersion: kiwix.jacaudi.dev/v1alpha1
kind: KiwixLibrary
metadata:
  name: library
  namespace: default
spec:
  storage:
    size: 100Gi
---
apiVersion: kiwix.jacaudi.dev/v1alpha1
kind: ZimArchive
metadata:
  name: wikipedia-en-100
  namespace: default
spec:
  libraryRef:
    name: library
  source:
    download:
      catalog:
        name: wikipedia_en_100
        flavour: full
```

`storage` takes exactly one of `size` (the operator creates a PVC) or
`existingClaim`. More manifests are in `config/samples/`.

### Check That It Worked

```bash
kubectl wait --for=condition=Available kiwixlibrary/library --timeout=5m
kubectl get zimarchive wikipedia-en-100 --watch
kubectl port-forward svc/library 8080:8080
```

The library is ready when `Available` is `True`. The archive is served when its
`Available` column reads `True`; then open
`http://localhost:8080/content/wikipedia-en-100`.

### Migrate, Rebuild and Uninstall

Details are in the [operations guide](docs/operations.md).

- **Migrating a library.** Set `adoptFile` on the `ZimArchive` that takes over
  an existing file, then remove it once the archive is served. Left in Git, it
  stalls disaster recovery on a fresh volume. See
  [migration](docs/migration.md).
- **Rebuilding a ZIM by hand.** Copy the file into `/data/.rebuild/` and `mv` it
  over `/data/<archive>.zim`, then restart the kiwix pod. Never stage it in
  `/data/.librarian/staged/`: garbage collection deletes anything there that
  belongs to no attempt. The full procedure is in the
  [operations guide](docs/operations.md#rebuilding-a-zim-by-hand).
- **Uninstalling.** Delete every `ZimArchive` first, then uninstall the chart.
  Finalizers need the running manager, so a namespace deleted after the
  uninstall waits on them.

## How It Works

Two CRDs, one image and one writer of the library. The operator image carries
the `manager`, `librarian` and `upload` subcommands and the pinned kiwix-tools
binaries, so kiwix-serve, `kiwix-manage` and the librarian cannot drift apart.

| Component | Runs as | Role |
|---|---|---|
| Manager | Deployment, leader election on | Reconciles both CRDs, schedules refreshes, looks up the catalog, applies the health gate, sends notifications |
| Librarian | Native sidecar in the kiwix pod | The only writer of `/data`: stages, verifies and promotes ZIMs, writes `library.xml`, downloads URLs |
| kiwix-serve | Container in the kiwix pod | Serves `/data` read-only and reloads `library.xml` live |
| Crawl Job | Job on any node | zimit crawls into scratch space, then the upload container streams the ZIM to the librarian |

Each refresh stages the new ZIM, compares it with the current one, and
promotes it only if it passes the gate. The manager owns policy and the
librarian owns files, and the librarian's HTTP API is the boundary between
them. See [architecture](docs/architecture.md).

## Documentation

**Guides**

- [Configuration](docs/configuration.md) — every field, chart value and manager flag
- [Archives](docs/archives.md) — crawl and download sources, refresh, gate, notifications
- [Migration](docs/migration.md) — adopt an existing library with `adoptFile`
- [Operations](docs/operations.md) — rebuilds, kept WARCs, upgrades, deletion, uninstall

**Reference**

- [Architecture](docs/architecture.md) — files, the name rule, attempts, crash safety
- [Security](docs/security.md) — NetworkPolicies, pod hardening, tokens
- [Development](docs/development.md) — tests, CI, releases

**Project**

- [Issues](https://github.com/jacaudi/kiwix-operator/issues) — bugs and questions
- [CI notes](.github/WORKFLOWS.md) — fork pull requests get no `ci` check

## Credits

- [Kiwix](https://kiwix.org): kiwix-tools 3.8.2 (`kiwix-serve`, `kiwix-manage`),
  bundled in the image under GPL-3.0, and the catalog and library format.
- [openZIM zimit](https://github.com/openzim/zimit): the crawler behind `crawl`.
- [bjw-s common library chart](https://github.com/bjw-s-labs/helm-charts): the
  base of the Helm chart.

## License

[Apache-2.0](LICENSE) © 2026 jacaudi. The bundled kiwix-tools binaries are
GPL-3.0; see [NOTICE](NOTICE).
