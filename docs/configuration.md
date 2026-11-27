# Configuration

This is a reference for every setting: the `KiwixLibrary` and `ZimArchive`
fields, the Helm chart values and the manager flags. For how to use them, see
[Archives](archives.md).

Both kinds are in the API group `kiwix.jacaudi.dev`, version `v1alpha1`. A
`ZimArchive` belongs to a `KiwixLibrary` in the same namespace.

## KiwixLibrary

Samples: [`config/samples/kiwix_v1alpha1_kiwixlibrary.yaml`](../config/samples/kiwix_v1alpha1_kiwixlibrary.yaml).

| Field | Default | Meaning |
|---|---|---|
| `spec.storage.existingClaim` | none | Adopt a PVC in the namespace. Exactly one of `existingClaim` or `size` |
| `spec.storage.size` | none | The operator creates the PVC `<library>-data` |
| `spec.storage.storageClassName` | cluster default | Used only with `size` |
| `spec.server.blockExternalLinks` | `false` | Passes `--blockexternal` to kiwix-serve |
| `spec.server.resources` | none | Resources for the kiwix-serve container |
| `spec.server.ingressFrom` | any | NetworkPolicy peers allowed to reach kiwix-serve; empty means any |
| `spec.route.parentRefs` | none | Gateway API parent references. Setting `route` creates an HTTPRoute `<library>` |
| `spec.route.hostnames` | none | Hostnames on the HTTPRoute |
| `spec.downloads.maxConcurrent` | `1` | Concurrent downloads (minimum 1) |
| `spec.crawls.maxConcurrent` | `1` | Concurrent crawl Jobs (minimum 1) |
| `spec.reserveSpace` | `5Gi` | Free space the librarian always keeps |
| `spec.notify.webhook.url` | none | Receives `{"title","message"}` JSON |

`storage` cannot change after creation. The librarian's `--max-fetches` and
`--reserve-space` arguments are part of the pod template, so changing
`downloads.maxConcurrent` or `reserveSpace` recreates the kiwix pod and briefly
interrupts kiwix-serve.

The operator creates, per library: a Deployment `<library>`, Services
`<library>` (port 8080) and `<library>-librarian` (port 8090), a Secret
`<library>-librarian-admin`, NetworkPolicies, an HTTPRoute when `route` is set,
and the PVC when `storage.size` is set. The PVC has no owner reference, so
deleting the library never deletes ZIM data.

Status: `books`, `freeBytes`, `unmanagedFiles` (only files that could not be
served, each with its `error`) and the conditions below.

| Type | Reasons |
|---|---|
| `Available` | `Serving`; `Starting`, `LibrarianUnreachable` |
| `Degraded` | `LowSpace`, `UnmanagedFileErrors`, `ReloadUnconfirmed`, `RouteUnsupported`, `ClaimInUse`; `Healthy` |

## ZimArchive

Samples:
[`kiwix_v1alpha1_zimarchive_crawl.yaml`](../config/samples/kiwix_v1alpha1_zimarchive_crawl.yaml)
and
[`kiwix_v1alpha1_zimarchive_catalog.yaml`](../config/samples/kiwix_v1alpha1_zimarchive_catalog.yaml).

The name must be a DNS label of at most 45 characters. The file is
`/data/<name>.zim` and is served at `/content/<name>`.

| Field | Default | Meaning |
|---|---|---|
| `spec.libraryRef.name` | required | The library; immutable |
| `spec.source.crawl` | none | Crawl a site. Exactly one of `crawl` or `download` |
| `spec.source.download` | none | Download a book |
| `spec.refresh.schedule` | empty | Cron expression; empty means fetch once, then on demand only |
| `spec.refresh.timeZone` | UTC | IANA time zone for the schedule |
| `spec.refresh.minArticleRatio` | `80` | Percent of the current article count a refresh must reach (0 to 100) |
| `spec.refresh.minSizeRatio` | `80` | Percent of the current size a refresh must reach (0 to 100) |
| `spec.adoptFile` | empty | An unmanaged file this archive takes over; see [Migration](migration.md) |
| `spec.notify.onSuccess` | `false` | Also notify when a refresh succeeds |
| `spec.deletionPolicy` | `Retain` | `Retain` or `Delete` |

Crawl source fields:

| Field | Default | Meaning |
|---|---|---|
| `seeds` | required | URLs to start from, joined with commas for zimit |
| `sitemap` | empty | Passed as `--useSitemap` |
| `scopeExcludeRx` | empty | Passed as `--scopeExcludeRx` |
| `workers` | `4` | Parallel workers, 1 to 16 |
| `title` | empty | ZIM title |
| `timeout` | `48h` | zimit `--timeSoftLimit`; at most 8760h. The crawl stops and still builds a ZIM |
| `keepWarcs` | `false` | Also store the raw WARCs; see [Operations](operations.md#keeping-and-reading-warcs) |
| `customCss` | empty | CSS injected into every page, for example to hide a banner |
| `scratch.size` | none | Size of the ephemeral scratch volume |
| `scratch.storageClassName` | cluster default | Scratch storage class |
| `resources` | none | Resources for the crawl container |

Download source fields: `catalog.name` and `catalog.flavour` (the flavour is
required when the name has more than one), or `url` with an optional `sha256`
(64 lowercase hex characters, valid only with `url`).

An on-demand refresh is started by setting the annotation
`kiwix.jacaudi.dev/requestedAt` to any value different from
`status.lastHandledRequestedAt`.

Status: `current` (file, `bookId`, `articleCount`, `sizeKiB`, `sha256`,
`sourceVersion`, `promotedAt`), `lastAttempt`, `nextRefreshAt`,
`failedAttempts` and the conditions below. `kubectl get zimarchive` shows
`Library`, `Version`, `Available` and `Age`.

| Type | Reasons |
|---|---|
| `Available` | `Served`; `NotYetFetched`, `AwaitingAdoption`, `LibraryUnavailable` |
| `Progressing` | `Crawling`, `Uploading`, `Downloading`, `Promoting`, `Queued`; `Idle` |
| `Degraded` | `CrawlFailed`, `UploadFailed`, `DownloadFailed`, `HashMismatch`, `GateRejected`, `InsufficientSpace`, `CatalogLookupFailed`, `DuplicateBook`, `AdoptFileMissing`, `AdoptFileInvalid`, `AdoptConflict`, `InvalidSchedule`, `LibraryUnavailable`; `Healthy` |

`CurrentLost` is a Warning Event, not a condition reason.

## Helm Chart Values

The chart is `oci://ghcr.io/jacaudi/charts/kiwix-operator`, built on the bjw-s
common library. Defaults are in [`chart/values.yaml`](../chart/values.yaml).

| Value | Default | Meaning |
|---|---|---|
| `image.repository` | `ghcr.io/jacaudi/kiwix-operator` | The operator image |
| `image.tag` | `v` + chart `appVersion` | Also used for the librarian and upload containers |
| `image.pullPolicy` | `IfNotPresent` | |
| `zimit.image` | digest-pinned zimit 3.1.3 | The crawl image |
| `catalogURL` | `https://opds.library.kiwix.org` | Kiwix OPDS catalog root |
| `webhookRetries` | `3` | Retries for a failed notification |
| `crawlEgressAllowCIDRs` | `[]` | CIDRs added to the kiwix and crawl pods' internet egress rules |
| `probeAllowCIDRs` | `[]` | CIDRs allowed to reach both kiwix pod ports, for CNIs that filter kubelet probes |
| `controller.replicas` | `1` | Leader election is always on, so extra replicas stand by |
| `controller.strategy` | `Recreate` | |
| `resources` | 10m / 128Mi requests, 256Mi limit | Manager container |
| `serviceAccount.create`, `.name`, `.annotations` | `true`, empty, `{}` | |
| `rbac.enabled` | `true` | Renders the ClusterRole and binding |

Do not set `global.nameOverride`: the NetworkPolicy that lets the manager reach
a librarian selects the manager pod by the label
`app.kubernetes.io/name: kiwix-operator`.

## Manager Flags

The chart sets these. Each has an environment variable except the first four.

| Flag | Environment | Default |
|---|---|---|
| `--metrics-bind-address` | | `:8080` |
| `--health-probe-bind-address` | | `:8081` |
| `--leader-elect` | | `false` |
| `--log-format`, `--log-level` | | `json`, `info` |
| `--catalog-url` | `CATALOG_URL` | `https://opds.library.kiwix.org` |
| `--operator-image` | `OPERATOR_IMAGE` | none |
| `--zimit-image` | `ZIMIT_IMAGE` | none |
| `--operator-namespace` | `POD_NAMESPACE` | none |
| `--webhook-retries` | `WEBHOOK_RETRIES` | `3` |
| `--crawl-egress-allow-cidrs` | `CRAWL_EGRESS_ALLOW_CIDRS` | empty |
| `--probe-allow-cidrs` | `PROBE_ALLOW_CIDRS` | empty |

The manager serves controller-runtime's default metrics on `:8080` and probes
on `:8081`. There are no operator-specific metrics.

[← Back to README](../README.md)
