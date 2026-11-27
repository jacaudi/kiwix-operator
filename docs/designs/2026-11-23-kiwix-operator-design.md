# kiwix-operator — design

Date: 2026-11-23 (revised 2026-11-24 after three design-gate passes) · Status: approved 2026-11-24
Repo: `github.com/jacaudi/kiwix-operator`, public (user decision, 2026-11-23: create the public repo, push the feature branch, open a draft PR, and archive `jacaudi/kiwix-helm-chart`). License: Apache-2.0, like the author's other operators, plus the GPLv3 notice for the bundled kiwix-tools binaries.

## Intent

Archive websites and published ZIM files into a self-hosted Kiwix library, declaratively, and keep them fresh.

Why now: more sites are going to be archived (the trigger was `arm.jonpalmisc.com`, offline on 2026-12-04 after a legal takedown), and each archive needs an option to be rechecked and re-downloaded. Today every site is a hand-written one-shot Job pinned to kiwix's node, kiwix needs a manual `rollout restart` to see a new ZIM, and nobody is told when a crawl finishes or fails.

The operator replaces `jacaudi/kiwix-helm-chart` (decided: option A). It runs kiwix-serve, crawls sites with openzim's zimit anywhere in the cluster, downloads published ZIMs from the kiwix.org catalog, swaps new versions in only when they look healthy, makes kiwix-serve pick them up without a restart, and notifies the user.

## Guiding principles and references

- **KISS / YAGNI:** two CRDs, one image, one writer of the library. No operator-specific metrics, no zimit flag passthrough, no multi-version retention until a present need exists. Choices a user may reasonably want either way (creating the HTTPRoute, keeping WARCs) are settings, not hard-coded calls (user direction, 2026-11-24).
- **DRY:** kiwix-tools is pinned once: the operator image's base. kiwix-serve, the librarian and `kiwix-manage` all run from that image, so they can't drift apart.
- **Patchbay:** each source kind (`crawl`, `download`) is its own unit behind a shared "stage a ZIM in the librarian" contract. A new source kind (for example the `peer` source under consideration) is a new file plus wiring.
- **SOLID:** the policy (health gate, schedule, catalog version) lives in the manager; the mechanics (staging, verification, promotion, `library.xml`) live in the librarian. The librarian's HTTP API is the boundary, and it exists because the two run in different pods.
- **12-Factor:** config via manager flags and env, set by the chart (catalog URL, default images, webhook retries); logs as structured JSON to stdout; the librarian's state is on the volume it owns, never in the manager.
- **Prior art:** kiwix's own production (`kiwix/operations`, `zim/library/browse/backend.yaml`) runs `ghcr.io/kiwix/kiwix-tools:3.8.2` as `kiwix-serve -b --library --monitorLibrary --nodatealias … catalog.xml`, fed by a separate catalog-retriever container. This design uses the same serving mode. openzim's zimfarm (recipe → scraper → separate uploader container) validates the upload shape but needs a Docker socket (see Approaches).

## Goals

1. Two CRDs: `KiwixLibrary` (one kiwix-serve instance) and `ZimArchive` (one archived thing).
2. `ZimArchive` sources: `crawl` (zimit) and `download` (kiwix.org catalog lookup by name, or a fixed URL).
3. Crawls run on any node with their own scratch volume and upload the finished ZIM over HTTP to a librarian sidecar in the kiwix pod. Nothing but the kiwix pod ever mounts the library volume.
4. Refresh on a cron schedule or on demand. A new version replaces the old one only if it passes the health gate; otherwise the old one stays and the user is told.
5. kiwix-serve picks up added, replaced and removed ZIMs live, with no pod restart, and each archive keeps a **stable URL** (`/content/<archive>`) across refreshes.
6. Notifications by webhook (`{"title","message"}` JSON) for failed, rejected and (opt-in per archive) succeeded attempts.
7. Migration path for the existing library: adopt the existing PVC and its ZIMs without re-downloading or losing anything.

## Non-goals

- Scrapers other than zimit (mwoffliner, youtube, sotoki…). See Patchbay above.
- Keeping multiple dated versions of one archive side by side (rejected refresh option B).
- An approval step before a swap (rejected refresh option C).
- A UI, authentication in front of kiwix-serve, or publishing ZIMs anywhere.
- **Federation between libraries** (user decision, 2026-11-24). Planned follow-up: a `peer` source kind (see Approaches).
- Operator-specific Prometheus metrics in v1 (status, conditions and Events cover visibility). The manager still serves controller-runtime's default metrics endpoint on `:8080`, and the chart publishes it as the `metrics` Service port; probes are on `:8081`.
- A passthrough for arbitrary zimit flags. Typed fields cover what is used today.
- The home-ops migration itself (a follow-up MR in `n7knightone/home-ops`; this design only guarantees the adoption features it needs).

## Anti-goals (a failure even if it technically works)

- **A refresh that replaces a good archive with a worse one.** A takedown page, a login wall or a truncated crawl must never displace the last good ZIM.
- **Deleting ZIM data implicitly.** Removing a CR, a field or the operator never deletes a file unless `deletionPolicy: Delete` says so.
- **Crawls that must run on kiwix's node**, or anything else that mounts the RWO library volume besides the kiwix pod.
- **A partially written ZIM ever being served**, or skipped as "already present" on the next run.
- **Needing a Docker socket, privileged pods, or a pod restart** to add a ZIM.

## Constraints

- Cluster: Talos, Kubernetes 1.35.10, Flux, Cilium, ceph-block (RWO) as the default storage class. No Docker socket on nodes.
- Language and layout: Go 1.27, controller-runtime 0.25, k8s libraries 0.37, following `jacaudi/nextdns-operator` `main` (`api/v1alpha1`, `cmd/`, `internal/controller`, `chart/` with CRDs synced in, `taskfile.yml`, release-please). CI follows the `github-actions-align` standard: local reusable workflows, a lowercase `taskfile.yml` contract, release-please, one required status check named `ci`. It runs on every push, and for an open PR's branch that is the PR's check. A pull request from a fork gets no `ci` check; that is accepted for a single-maintainer repo and documented in `.github/WORKFLOWS.md` (a `pull_request` trigger with a job-level `if` would leave a skipped duplicate `ci` that counts as passing).
- API group `kiwix.jacaudi.dev`, version `v1alpha1`.
- kiwix-tools 3.8.2 (current; 3.8.1 fixed the `kiwix-manage remove` bug seen in 3.8.0).
- Default images (the operator image and the zimit image) are **manager flags set from chart values**. The zimit image is pinned by digest and tracked by Renovate. The operator image is referenced by tag, which defaults to the chart's `appVersion` and has no digest (it is the image this release builds). They are never CRD `default:` markers, which would freeze an image into each CR at creation.
- Notifications must work with the existing chaski service: `POST http://chaski.tools.svc.cluster.local:8080/hooks/<route>` with `{"title","message"}`, which forwards to Pushover (the pattern ytdl-api uses). A `kiwix` route in chaski is home-ops config, not operator code.

## Approaches considered

**How to build it (picked: 1).**

1. **Go operator** (picked). Matches the user's existing operators; the only option that delivers health-gated swaps, catalog refresh, per-archive status and notifications as code with tests.
2. **kro template, CronJobs and shell.** The health gate, catalog lookup and notifications become shell inside Jobs with no status to read: the fragility the chart review found in today's downloader.
3. **Self-host openzim's zimfarm.** Its worker README: "A Zimfarm worker is just a Docker container spawning other containers". Talos has no Docker socket, and it brings a Postgres-backed API and UI nobody asked for.

No existing Kubernetes operator for kiwix or zimit was found (GitHub search, 2026-11-23).

**Scope (picked: A).** The operator replaces kiwix-helm-chart. B (operator only crawls; chart gains the sidecar) was rejected: two writers of one library, and the chart's downloader bugs remain.

**Moving finished crawls (user's idea, adopted).** Crawls run anywhere and upload to an HTTP receiver beside kiwix.

**Refresh semantics (picked: A).** Replace only when the new version passes the health gate.

**Downloads (picked: A).** Catalog lookup by name; fixed URLs still allowed. Evidence: the HelmRelease's fixed URL `stardewvalleywiki_en_all_2026-01.zim` already returns 404; the catalog resolves the current `…_2026-10.zim`.

**Federation (user decision, 2026-11-24: not in v1).** Syncthing was evaluated as a federation backend and not recommended: no supported Go API (`lib/` is internal per the maintainers; REST "subject to change"), only per-block hashes, deletes propagate, and it adds a second identity system. The planned follow-up is a `peer` source kind that pulls from another library's librarian over the same verified download path.

## Verified facts the design rests on

| Fact | How it was checked |
|---|---|
| `kiwix-serve --library … --monitorLibrary` exits when `library.xml` is missing | docker run on 3.8.0; kiwix-serve 3.8.2 L297-299 |
| It starts and serves with a zero-book `<library version="20110515"></library>` | docker run; kiwix-serve 3.8.2 L302-305 |
| A `kiwix-manage add` is served within 2 s, no restart | docker run; catalog count 0 → 1 |
| An atomic rename-replace of `library.xml` is followed both ways | docker run; count 1 → 0 → 1 |
| A malformed replacement is rejected; the last good library keeps serving | docker run; libkiwix 14.2.0 manager.cpp L355-370 |
| **The monitor polls every 1 s and compares `st_mtime` in whole seconds with a strict `>`; the baseline is read after the first load.** Two rewrites inside one second can be missed | kiwix-serve 3.8.2 L144-156, L297-305, L320, L422-436 (design gate) |
| libkiwix opens archives lazily through an LRU cache of about 10% of the books, so an old file is usually not held open | library.cpp L102, L129-134, L391-401 (design gate) |
| A book's URL comes from its filename (`/content/<stem>`) | libkiwix book.cpp L190-202, name_mapper.cpp (design gate) |
| `kiwix-manage remove` is broken in 3.8.0 only; fixed in kiwix-tools 3.8.1 (PR #783, merged 2025-11-17); 3.8.2 released 2026-03-02 | docker run; GitHub releases and PR |
| `kiwix-manage add` records `articleCount`, `mediaCount`, `size` (KiB) in `<book>` | docker run; book.cpp L80-82, L120 |
| libzim's `articleCount` counts entries flagged FRONT_ARTICLE, and "depends of the ZIM archive" | libzim archive.h L302-314 (design gate) |
| The old bg3 crawl's `articleCount` (967) equals its `text/html` count (967) | live cluster |
| Catalog lookup by name works; `&flavour=` is ignored by the server, so filtering is client-side | curl (design gate tested the flavour parameter) |
| The `.meta4` carries `<size>` and `<hash type="sha-256">` | curl |
| zimit 3.1.3: `curl` present, uid 0, no `USER` line, entrypoint `entrypoint.sh` (appends to `/etc/hosts`); `--seeds` is one comma-separated string; `--timeSoftLimit` stops the crawl and still builds a ZIM, while `--timeHardLimit` exits without one | docker run; zimit.py L134-138, L566 |
| Native sidecars are stable since Kubernetes 1.33; the next container starts only after the sidecar's startupProbe succeeds; a sidecar's readinessProbe counts toward Pod readiness | Kubernetes docs, KEP-753 |
| ARM crawl: ~1 page/s with 4 workers | live Job logs |
| chaski webhook contract `{"title","message"}` at `/hooks/<route>` | home-ops chaski and ytdl-api HelmReleases |
| `/catalog/v2/entries` returns 10 entries unless `count` is given; `/catalog/v2/entry/<id>` returns 404 for an absent book | libkiwix 14.2.0 internalServer.cpp L1173, internalServer_catalog.cpp L130-132, L176-181 (design gate) |
| A ZIM's book id is its UUID; libzim rejects truncated files on open | libkiwix book.cpp L68; libzim 9.5.0 fileimpl.cpp L227-231 (design gate) |
| EndpointSlice `serving` follows Pod readiness; ReadWriteOnce allows several pods on one node | Kubernetes docs (endpoint-slices, persistent-volumes) (design gate) |
| zimit keeps WARCs in a root-only `mkdtemp` directory unless `--build` is given; `--warcs` accepts a URL to a tar | zimit.py v3.1.3 L767-772, L801-807 (design gate) |
| The other crawler in `selfhosted` limits egress to DNS plus 0.0.0.0/0 except RFC1918 | home-ops `archiveteam/app/networkpolicy.yaml` L24-28 |

## Design

### Components

```text
kiwix-operator (Deployment, 1 replica, leader election on)
  manager      reconciles KiwixLibrary and ZimArchive; schedules; catalog lookups; notifications

KiwixLibrary pod (Deployment, 1 replica, Recreate)
  librarian    native sidecar (initContainer, restartPolicy: Always); owns /data and library.xml
  kiwix-serve  starts after the librarian's startupProbe passes

Crawl attempt (Job, any node)
  zimit        initContainer; crawls into a generic ephemeral scratch volume
  upload       container; streams the ZIM to the librarian
```

**One image**, `ghcr.io/jacaudi/kiwix-operator`, built FROM `ghcr.io/kiwix/kiwix-tools:3.8.2` (pinned by digest) plus the Go binary with subcommands `manager`, `librarian` and `upload`. kiwix-serve runs from this same image (`kiwix-serve` is in kiwix-tools), so the server and `kiwix-manage` are always the same version and there is no `server.image` field.

### `KiwixLibrary`

```yaml
apiVersion: kiwix.jacaudi.dev/v1alpha1
kind: KiwixLibrary
metadata: {name: wiki, namespace: selfhosted}
spec:
  storage:                       # exactly one of (CEL); immutable
    existingClaim: kiwix         #   adopt a PVC (migration)
    # size: 100Gi                #   or the operator creates one
    # storageClassName: ceph-block   # omitted = cluster default; never sent as ""
  server:
    blockExternalLinks: false    # kiwix-serve --blockexternal
    resources: {}
    ingressFrom: []              # NetworkPolicyPeers allowed to the kiwix-serve port; empty = any
  route:                         # optional; omitted = no HTTPRoute (expose the Service yourself)
    parentRefs:                  # Gateway API ParentReference, passed through as is
      - {name: internal, namespace: network, sectionName: https-jacaudi}
    hostnames: [wiki.jacaudi.dev]
  downloads:
    maxConcurrent: 1
  crawls:
    maxConcurrent: 1
  reserveSpace: 5Gi              # free space the librarian always keeps
  notify:
    webhook:
      url: http://chaski.tools.svc.cluster.local:8080/hooks/kiwix
status:
  observedGeneration: 3
  conditions: [Available, Degraded]
  books: 11
  freeBytes: 99000000000
  unmanagedFiles: [{name: broken.zim, error: "kiwix-manage add failed: …"}]   # only files that could not be served; `error` is always set
```

The operator creates:
- the Deployment above;
- Service `<library>` (port `http` 8080 → kiwix-serve; the route target);
- Service `<library>-librarian` (librarian port only), with `publishNotReadyAddresses: true`. EndpointSlice `serving` follows Pod readiness, so without this the librarian would vanish from its Service whenever kiwix-serve isn't ready, breaking the manager and in-flight uploads;
- Secret `<library>-librarian-admin`, the manager's token for the librarian API;
- NetworkPolicies (below);
- HTTPRoute `<library>` → Service `<library>`, backend port `8080` (Gateway API takes a port number, not a name), only when `route` is set. If the Gateway API CRDs aren't installed, the library reports `Degraded/RouteUnsupported` and keeps serving; the manager doesn't fail to start. When `route` is removed, the controller deletes the HTTPRoute explicitly (an owner reference alone doesn't do that);
- the PVC `<library>-data` when `storage.size` is set. The PVC gets **no** owner reference, so deleting the `KiwixLibrary` never deletes ZIM data, and re-creating a library with the same name re-uses the PVC.

**One writer, enforced:** the librarian holds an exclusive `flock` on `/data/.librarian/lock` for its whole life and refuses to start without it. The manager also rejects a second `KiwixLibrary` naming a claim another library already uses (`Degraded/ClaimInUse`; the older library wins, then the lower name). A library that loses its claim to an older one has the Deployment it controls deleted, so no second pod can mount the claim; its Services, Secret and NetworkPolicies stay. ReadWriteOnce alone allows several pods on one node, so it isn't enough.

**Probes:**
- Librarian: a `startupProbe` that passes once the lock is held and `library.xml` is rendered, plus a `livenessProbe`. **No readinessProbe**, because a sidecar's readiness counts toward Pod readiness and would take kiwix-serve out of its Service.
- kiwix-serve: a `readinessProbe` on `/catalog/search?count=1`, as kiwix's own production uses.

**Kiwix pod security:** `runAsNonRoot`, `runAsUser: 1000`, `runAsGroup: 1000`, `fsGroup: 1000` with `fsGroupChangePolicy: OnRootMismatch` (the existing PVC was written with fsGroup 1000), all capabilities dropped, no privilege escalation, `RuntimeDefault` seccomp, read-only root filesystem with an `emptyDir` at `/tmp`, `automountServiceAccountToken: false`. `/data` is mounted **read-only** into kiwix-serve, read-write only into the librarian. The user is set explicitly because the kiwix-tools image has no `USER` line, so it runs as root; only the separate kiwix-serve image sets one (uid 1001 in 3.8.2).

**Librarian resources:** requests 50m CPU and 128 Mi memory; limit 1 Gi, since `kiwix-manage add` opens large ZIMs.

**kiwix-serve arguments:** `--library /data/library.xml --monitorLibrary --port 8080`, plus `--blockexternal` when set. Date aliases stay off, the default, because filenames are stable (below). The flag `--nodatealiases` *enables* them despite its name; it is never passed.

**Accepted cost of one image:** every operator release changes the image, which recreates the kiwix pod and briefly interrupts kiwix-serve. The librarian's `--max-fetches` (from `downloads.maxConcurrent`) and `--reserve-space` (from `reserveSpace`) are pod-template arguments too, so changing either field recreates the pod with the same brief interruption.

### `ZimArchive`

```yaml
apiVersion: kiwix.jacaudi.dev/v1alpha1
kind: ZimArchive
metadata: {name: arm-reference, namespace: selfhosted}   # name: DNS label, ≤ 45 chars
spec:
  libraryRef: {name: wiki}       # same namespace only; immutable
  source:                        # exactly one of (CEL)
    crawl:
      seeds: [https://arm.jonpalmisc.com/]   # joined with "," for zimit --seeds
      sitemap: ""                # zimit --useSitemap
      scopeExcludeRx: ""         # zimit --scopeExcludeRx
      workers: 4
      title: ""
      timeout: 48h               # zimit --timeSoftLimit: stops crawling and still builds a ZIM (the ratio gate catches truncation)
      keepWarcs: false           # also upload the raw WARCs and keep them (see Crawl attempts)
      customCss: ""              # CSS injected into every page (zimit --custom-css), e.g. hide a banner
      scratch: {size: 20Gi}      # storageClassName optional; omitted = cluster default
      resources: {}
    # download:                  # exactly one of (CEL)
    #   catalog: {name: wikipedia_en_100, flavour: full}
    #   url: https://…/file.zim
    #   sha256: ""               # url only, optional
  refresh:
    schedule: ""                 # cron; empty = fetch once, then on demand only
    timeZone: ""                 # IANA; empty = UTC
    minArticleRatio: 80          # percent of the current articleCount
    minSizeRatio: 80             # percent of the current size
  adoptFile: ""                  # an unmanaged file this archive takes over (migration)
  notify: {onSuccess: false}
  deletionPolicy: Retain         # Retain | Delete
status:
  observedGeneration: 2
  conditions: [Available, Progressing, Degraded]
  current: {file: arm-reference.zim, bookId, articleCount, sizeKiB, sha256, sourceVersion, promotedAt}
  lastAttempt: {id, kind: upload|fetch, trigger: initial|schedule|requested|backoff, requestedAt, scheduleTick,
                startedAt, finishedAt, deadline, jobCreated, sourceVersion,
                result: Succeeded|Rejected|Failed|Unchanged, reason, message, notified}
  lastFailureNotifiedAt: …       # rate-limits failure notifications
  failedAttempts: 0              # consecutive failures while there is no current version (drives backoff)
  lastHandledRequestedAt: "…"
  lastScheduledAt: …             # the last schedule tick acted on
  nextRefreshAt: …
  finalizerFirstFailureAt: …     # when deletion first failed or stayed pending (times the 10-minute give-up)
  adoptMissingNotified: false    # the missing-adoptFile notification went out
```

An attempt ends `Unchanged` with an empty `id` and `startedAt` equal to `finishedAt`, so it is never open. `kind` is the librarian's attempt kind (below); `trigger` says why the attempt started, and `requestedAt` and `scheduleTick` record the annotation value or tick that did, so a trigger is consumed exactly once. `sourceVersion` on an attempt is the catalog version a fetch downloads, recorded before registration so the promotion can name it without asking the catalog again.

**Validation and defaults** (CEL and OpenAPI markers on the CRDs):
- `crawl.workers` is 1 to 16 (default 4).
- `crawl.timeout` matches a Go-duration pattern, must be a duration of at most 8760h, and defaults to 48h; the controller treats `0s` as that default.
- `minArticleRatio` and `minSizeRatio` are 0 to 100 (default 80).
- `maxConcurrent` is at least 1 (default 1).
- `download.sha256` is 64 lowercase hex characters.
- `storage.existingClaim`, `download.url` and `libraryRef.name` are never empty strings, and an empty `storageClassName` is treated as unset.

**`sourceVersion`** is always normalized to `YYYY-MM`, and the manager computes it per source:
- **catalog:** the entry's version, from its file name's `_YYYY-MM` suffix, else the month of the entry's `<updated>` date (the live feed has no `<version>`);
- **url:** the `_YYYY-MM` in the URL's file name, else the promotion month;
- **crawl:** the promotion month (UTC), the single source of truth in the manager, since it is the crawl's month in practice (a month off at a month boundary is the accepted cost). The ZIM's `Date` metadata is not read for crawls or URLs;
- **adopted file:** its file name's suffix, else the month of the book's `date` attribute.

**`sourceFingerprint`** identifies where the current version came from: `crawl:<sha256 of the sorted seeds>`, `catalog:<name>/<flavour>`, or `url:<url>`. It is stored with the current version. The pre-download checks only apply when it matches the spec, so switching a catalog archive from `maxi` to `nopic` in the same month fetches instead of reporting `Unchanged`. An archive that inherits a retained file from a different source therefore always refetches. The health gate still applies; to accept a deliberately smaller new source, set both ratios to 0 for one refresh.

**Conditions and reasons:**

| Kind | Type | Reasons |
|---|---|---|
| ZimArchive | `Available` | True: `Served`. False: `NotYetFetched`, `AwaitingAdoption`, `LibraryUnavailable` |
| ZimArchive | `Progressing` | True: `Crawling`, `Uploading`, `Downloading`, `Promoting`, `Queued`. False: `Idle` |
| ZimArchive | `Degraded` | True: `CrawlFailed`, `UploadFailed`, `DownloadFailed`, `HashMismatch`, `GateRejected`, `InsufficientSpace`, `CatalogLookupFailed`, `DuplicateBook`, `AdoptFileMissing`, `AdoptFileInvalid`, `AdoptConflict`, `InvalidSchedule`, `LibraryUnavailable`. False: `Healthy` |
| KiwixLibrary | `Available` | True: `Serving`. False: `Starting`, `LibrarianUnreachable` |
| KiwixLibrary | `Degraded` | True: `LowSpace`, `UnmanagedFileErrors`, `ReloadUnconfirmed`, `RouteUnsupported`, `ClaimInUse`. False: `Healthy` |

Notes on the table:
- **No `Adopting` and no `CurrentChanged` reason.** Adoption completes inside one reconcile, so a `Progressing=Adopting` state is never observable. After a second `409 CurrentChanged` the attempt stays staged and the controller polls again in 15 s, so there is nothing to report as `Degraded`.
- **`InvalidSchedule`** is set for a bad cron expression, a bad time zone or a `CRON_TZ=` prefix, and clears when the spec is fixed; no requeue happens until the spec changes.
- **`Degraded/LibraryUnavailable`** is carried from an attempt that failed because its library was gone, or because its librarian was unreachable for an hour past the attempt's deadline. While the library is merely unreachable, `Available=False/LibraryUnavailable` is the signal.
- **`CurrentLost`** is a Warning Event reason, not a condition reason (below).

No `status.phase` field (Kubernetes API conventions discourage it).

**On-demand refresh:** setting the annotation `kiwix.jacaudi.dev/requestedAt` to a value different from `status.lastHandledRequestedAt` starts an attempt. This is Flux's `reconcile.fluxcd.io/requestedAt` pattern.

**When an archive is due** (evaluated after adoption, below):
- it has no current version and has never had an attempt; or
- it has no current version, its last attempt failed or was rejected, and its backoff has elapsed (1 h, then 4 h, then every 24 h, counted by `failedAttempts`); or
- a schedule tick arrives; or
- `requestedAt` changed.

**Other scheduling rules:**
- One attempt at a time per archive.
- A schedule tick during a running attempt is skipped, with an Event.
- A `requestedAt` change during an attempt is handled once that attempt ends.
- Ticks missed while the operator was down cause one attempt at startup, not a backfill.
- **Failure notifications:** the first failure always notifies; repeats during backoff notify at most once per 24 h.
- **A lost current version:** each reconcile mirrors the librarian's current book into `status.current`. When the librarian reports none but `status.current` is set, for example after the file was removed by hand, the manager clears `status.current` and `status.lastAttempt` and records a `CurrentLost` Warning Event. The archive then counts as never attempted: it is due at once with trigger `initial`, the first-version gate applies (`articleCount ≥ 1`, not the refresh ratios), and the failure backoff starts over. A lost current therefore puts the archive back under the first-version gate.

### Files, ownership and crash safety

**The name rule (canonical, unconditional).** `/data/<archive>.zim`, where `<archive>` is a DNS label, belongs to the `ZimArchive` of that name in the library's namespace, however the file got there: promotion, adoption, a `Retain` deletion, or a rebuilt manifest. If that file exists and `kiwix-manage add` accepts it, it **is** that archive's current version.
- Re-creating a deleted archive therefore inherits the retained file. Its next fetch is a *refresh*, so the health gate's ratios apply, and a takedown page can't overwrite it.
- Its URL is `/content/<archive>` for life.
- Rolling back to the chart's `/data/*.zim` glob still serves it.

**Unmanaged files** are every other top-level `*.zim`: names that aren't a DNS label, such as `wikipedia_en_100_2025-10.zim`. They are served as is. A file that fails `kiwix-manage add` (libzim rejects truncated files) is listed in `status.unmanagedFiles` with its error and not served. `status.unmanagedFiles` lists only those failures; a healthy unmanaged file is served and not listed. A file named like a DNS label but longer than 45 characters (46 to 63) cannot be an archive's file, so it is also an unmanaged file: served silently when it is healthy, listed only if it fails. The librarian scans `/data` at startup.

**The manifest** (`/data/.librarian/manifest.json`, written temp + fsync of the file and its directory + rename) records:
- **every served book**, managed and unmanaged, as its full `<book>` element from `kiwix-manage add`, with the `path` rewritten to the absolute `/data/<file>`. kiwix-serve trusts `library.xml` for every attribute (title, language, name, flavour, tags, favicon), and `kiwix-manage` writes paths relative to its scratch library;
- for each archive name, its current file, book id (ZIM UUID), counts, sha256, source version and **source fingerprint** (below);
- pending intents, pending unlinks, and attempt registrations with their states and deadlines.

It supplies the health-gate baseline; `status.current` mirrors it. If it is lost, it is rebuilt from disk by the name rule. Registrations are then lost, and in-flight attempts fail and retry. **Disk wins over the manifest:** each scan reads the 16-byte header UUID of every managed file and corrects the manifest when they disagree.

**Rebuilding a ZIM by hand** (for example from kept WARCs) is out of scope for the operator. The documented procedure is: copy the new file into a directory of your own on the same volume that the librarian never reads or cleans, such as `/data/.rebuild/`, `mv` it from there over `/data/<archive>.zim` (a rename within one volume is atomic), then restart the kiwix pod so the startup scan picks it up. Never use `/data/.librarian/staged/` for this: the librarian's garbage collection deletes any file there that belongs to no registered attempt, at startup and hourly, so a copy in progress could be lost. A `kubectl cp` straight into the served path is a non-atomic write and is not supported.

**Staging and WARC state** live under `/data/.librarian/` (`staged/` for attempts' parts and partial files, `warcs/` for kept WARC tars), which no glob reaches.

**Every `library.xml` write** goes through one serialized routine:

```text
1. render library.xml from the manifest to a temp file
2. set its mtime to max(now, previous library.xml mtime + 1 s); rename over library.xml
3. confirm: for each book added or changed, GET /catalog/v2/entry/<bookId> returns 200;
           for each book removed, it returns 404 (poll every 1 s, up to 30 s)
4. not confirmed → repeat from 1 (new mtime), at most 3 times;
   then set the library's Degraded/ReloadUnconfirmed and continue (the files are already in place;
   the next write or a restart reloads)
```

This covers every rewrite: promotion, adoption, deletion, startup recovery and scans. Whole-second `st_mtime` with a 1 s poll can miss a write that lands right after kiwix-serve's first load, which is why every write is confirmed, not just promotions. Confirmation checks each changed entry's `/content/…` link as well as its id, because adoption keeps the id and changes only the path.

**Pending unlinks:** a file removed by `Delete` is unlinked only after its book's absence is confirmed, so kiwix-serve never holds a book whose file is gone. Until then it is recorded as a pending unlink in the manifest and retried after the next confirmed write. `ReloadUnconfirmed` clears on the next confirmed write.

**Promotion and adoption protocol (librarian):**

```text
0. idempotency: a promote for an attempt already in state "promoted" returns 200 immediately
1. compare-and-swap: the manifest's current bookId for <archive> must equal the request's expected current
   (or both "none"); otherwise answer 409 CurrentChanged, write nothing, and return the attempt to "staged"
2. intent {op: promote|adopt, archive, attempt?, source path, expected current bookId, new bookId, <book> element}
   written to the manifest (temp, fsync, rename)
3. rename source (staged/<attempt>.zim, or the adopted file) over /data/<archive>.zim
4. library.xml write routine (above)
5. commit: manifest current = new; attempt state "promoted"; clear intent
```

- **Compare-and-swap** guarantees the file being replaced is the one the health gate measured against. The librarian never renames over `/data/<archive>.zim` in any other way. The expected current bookId is the one the manager read from the librarian (`GET /v1/attempts/<attempt>`, which returns the staged counts and the current book in one response) and gated against.
- **After a 409:** the manager re-reads the current version and re-gates the staged ZIM against it. Pass → promote with the new expected id. Fail → discard, with result `Rejected`.
- **Startup order:**
  1. Take the lock.
  2. Load the manifest. If there is none (a first start, or a lost manifest), scan `/data` now, synchronously, to build it. This is a header read and one `kiwix-manage add` per file, so an adopted library comes up with all its books instead of empty.
  3. Render `library.xml` from the manifest's `<book>` elements without confirming (managed and unmanaged books both survive a librarian restart).
  4. Pass the startupProbe. kiwix-serve starts.
  5. Only then roll forward intents and pending unlinks, and run the scan, each through the confirmed write routine. Recovery never blocks startup.
- **Roll-forward:**
  - **First, re-check the compare-and-swap** against the intent's expected bookId. On mismatch, drop the intent and leave the source where it is; nothing is renamed.
  - If the intent's source file still exists, resume at step 3.
  - If it's gone and `/data/<archive>.zim`'s ZIM header UUID (bytes 8–24) equals the intent's new bookId, resume at step 4. This is a 16-byte read, not a hash of a 100 GB file.
  - Otherwise drop the intent.
- **Rename-over while serving:** the old inode stays valid for any handle libkiwix holds. libkiwix drops a book's cache entry when the book is removed or replaced, so readers see the old version or the new one, never a mix.
- **Duplicate books:** promoting a ZIM whose UUID is already served, by another archive **or by an unmanaged file**, is rejected (`DuplicateBook`). libkiwix would otherwise treat the second entry as an update of the first. The message names the colliding file and suggests `adoptFile`. For example, a `catalog` archive for DevDocs Python would collide with the unmanaged `devdocs_en_python_2026-10.zim` already on the volume.

**`adoptFile`** (migration):
- **Ordering:** adoption is evaluated **before** the due check. While `adoptFile` is set and the archive has no current version, the archive **never fetches**.
- **Validation:** the name must be a basename matching `^[A-Za-z0-9][A-Za-z0-9._-]*\.zim$` (no `/`, no NUL, no leading `.`), and the file must be an existing unmanaged top-level regular file. `lstat` is used, so symlinks are rejected.
- **Failure states:** a missing file sets `Degraded/AdoptFileMissing` and `Available=False/AwaitingAdoption`; an invalid name sets `AdoptFileInvalid`. In both cases the archive waits; it doesn't fall through to a fetch. To fetch instead, clear the field.
- **Adoption** runs the protocol above with `op: adopt`: the file is renamed to `/data/<archive>.zim`, a one-time URL change at migration.
- **Two archives claiming one file:** the older `ZimArchive` (by creation time, then by name) wins; the other gets `Degraded/AdoptConflict` and waits.
- **Notification:** `AdoptFileMissing` notifies once. The docs say to remove `adoptFile` after migration; left in Git, it would stall a disaster recovery on a fresh volume.
- **Afterwards:** once the archive has a current version, the field is ignored.

### Attempt lifecycle

```text
due
  → pre-checks, before anything is registered or downloaded:
       (only when current.sourceFingerprint equals this spec's fingerprint; otherwise go straight to fetching)
       catalog: look up the newest matching entry; not strictly newer than current.sourceVersion → Unchanged, end
       url with spec.sha256: equal to current.sha256 → Unchanged, end
       manager concurrency: crawls.maxConcurrent / downloads.maxConcurrent reached → Queued, retry later
  → manager generates the attempt ID and upload token; writes status.lastAttempt.id (status update)
  → crawl only: manager writes Secret <archive>-<attempt>-upload (owned by the ZimArchive; deleted when the attempt ends)
  → manager registers: POST /v1/attempts {archive, attempt, kind: upload|fetch, tokenHash?, parts: [zim] or [zim, warcs],
                                          fetch: {url, sha256, size}?, expectedCurrentBookId}
       idempotent on (archive, attempt): a repeat returns the existing registration
  → crawl: manager creates the Job;  download: the librarian fetches
  → librarian stages each part: .part → free-space checks → SHA-256 → staged; attempt is "staged" when every part is in;
       then kiwix-manage add on a scratch library → counts
  → manager gates:
       url without spec.sha256: staged sha256 equal to current → Unchanged (discard)
       first version: articleCount ≥ 1
       refresh: articleCount ≥ minArticleRatio% AND sizeKiB ≥ minSizeRatio% of current
  → promote (compare-and-swap protocol) | discard
  → status, Event, notification (once per attempt; lastAttempt.notified guards repeats)
```

**The librarian's attempt state is authoritative.** A Job that fails after its upload completed doesn't fail the attempt: the staged ZIM wins. If an attempt's Job has already been TTL-deleted, the manager reads the librarian's state.

**Librarian attempt states** (persisted in the manifest):

```text
registered → receiving → staged → promoting → promoted | discarded | failed
     ↑            │
     └────────────┘  on disconnect, 2 min with no bytes read, or librarian restart: the partial part is deleted
```

- **Uploads restart from zero** within the post-crawl budget; there is no upload resume.
- **Part uploads** carry `Content-Length` and a declared `X-Content-SHA256` header; the librarian verifies the body against it. A PUT for a part already received answers 200 if the declared hash matches (so a retry after a lost response succeeds), 409 if it doesn't. The WARC tar is written once to `/output/warcs.tar` in scratch (size `scratch` for it), so its length and hash are known before upload and retries send identical bytes. It holds only the `build/collections/*/archive` directories, which are the WARC files (see Crawl attempts).
- **Download intent** (URL, sha256, `.part` path) is persisted, so HTTP Range resume survives a librarian restart.
- **Attempt ID format:** 8 lowercase base32 characters. The Job name is `<archive>-<attemptID>`, at most 54 characters given the 45-character archive name limit.

### Librarian API and security

Attempt kinds are `upload` (bytes arrive from a Job) and `fetch` (the librarian pulls a URL), not crawl/download. A future `peer` source is then a manager change only.

| Endpoint | Caller | Auth |
|---|---|---|
| `POST /v1/attempts` | manager | admin token |
| `GET /v1/attempts/<attempt>` | manager | admin token |
| `POST /v1/attempts/<attempt>/promote`, `/discard` | manager | admin token |
| `PUT /v1/attempts/<attempt>/parts/zim`, `/parts/warcs` | crawl Job's upload container | that attempt's token |
| `GET /v1/archives/<archive>` | manager, on every reconcile, to mirror the archive's current book into status | admin token |
| `POST /v1/archives/<archive>/adopt` `{file}` | manager | admin token |
| `DELETE /v1/archives/<archive>?policy=Retain\|Delete` | manager | admin token |
| `GET /v1/archives/<archive>/warcs` | manager, or a user via port-forward | admin token |
| `GET /v1/library` | manager | admin token |
| `GET /healthz/startup`, `GET /healthz/live` | kubelet probes | none |

- **Path segments** (`<archive>`, `<attempt>`) must match `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` before any filesystem use. Encoded slashes are rejected. `adoptFile` names use their own rule (above).
- **Per-attempt upload token:** generated by the manager; only its hash is sent to and stored by the librarian. It lives in the attempt's Secret, which is mounted **only into the upload container**, never into zimit's.
- **What the token doesn't stop (accepted risk):** a compromised Chromium could make zimit write a crafted ZIM that the upload container then sends. The health gate runs *after* libzim parses the file, so it can't stop a parser exploit. `kiwix-manage add` runs locked down (non-root, no capabilities, read-only root filesystem), but the librarian has read-write `/data`: a libzim exploit there means write access to the library. Accepted for v1; isolating the parse in a container with staging mounted read-only is a planned hardening.
- **NetworkPolicies:**
  - **Kiwix pod ingress:**
    - the kiwix-serve port from `server.ingressFrom` (empty means any);
    - the librarian port only from the operator's pods (namespaceSelector plus podSelector) and this library's crawl pods (`kiwix.jacaudi.dev/library: <name>`).
  - **Kiwix pod egress** (the librarian fetches user-supplied URLs and follows redirects to mirrors): DNS (UDP and TCP 53), and `0.0.0.0/0` except the private ranges below.
  - **Crawl pod egress:** DNS (UDP and TCP 53); the librarian port on this library's pod; and `0.0.0.0/0` except `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10` and `169.254.0.0/16`. This mirrors the archiveteam convention (which allows only UDP 53) and stops a hostile page from reaching in-cluster services.
  - **Test-only flag:** `--crawl-egress-allow-cidrs` (default empty) adds CIDRs to both egress allow-lists. The kind e2e sets it, because its test site is served in-cluster.
  - **Probe flag:** `--probe-allow-cidrs` (`PROBE_ALLOW_CIDRS`; chart value `probeAllowCIDRs`; default empty) adds an ingress rule that lets those CIDRs reach both kiwix pod ports, serve and librarian, for CNIs that filter kubelet probes through the policy. The kind e2e sets it to the kind docker subnet.
  - **IPv6:** the policies assume an IPv4-only cluster (see Open questions).
  - **Crawl and kiwix pods:** `automountServiceAccountToken: false`.
- **Webhook URL:** set by whoever can edit the `KiwixLibrary`. That's the same trust as creating pods in its namespace, and in-cluster targets such as chaski must work, so the manager doesn't filter it.
- **Crawl pod security (user decision, 2026-11-23: "root, but locked down"):**
  - zimit may run as root; it drops all capabilities, sets `allowPrivilegeEscalation: false`, uses `RuntimeDefault` seccomp, and is never `privileged`.
  - **Probe result (2026-11-25, `docs/notes/zimit-locked-down.md`):** zimit 3.1.3 crawls and writes its ZIM with every capability dropped, so no capability is added back (`zimitExtraCapabilities` is empty in code). The probe also found that zimit's crawler defaults to `--diskUtilization 90` and exits before fetching anything on a volume over 90% full, and that the root-owned `0700` profile directories under `build/` are why only the WARC directories are tarred (below).
  - The crawl pod sets `fsGroup: 1000`, so the non-root upload container can read what root zimit wrote on the scratch volume (new files inherit group 1000).
  - The upload container runs as non-root uid 1000 with the same restrictions.

### Disk space

- Before accepting any part (the `.meta4` `<size>`, Content-Length, or the declared length), the librarian requires `free ≥ size + reserveSpace`. A refresh needs room for the current file and the new one at once. The librarian re-checks while writing and aborts at the reserve (`InsufficientSpace`).
- Concurrency is enforced by the manager, which doesn't register attempts beyond `crawls.maxConcurrent` or `downloads.maxConcurrent`. The librarian also enforces the fetch limit itself, with a semaphore sized by `--max-fetches` (`downloads.maxConcurrent`), as a second line of defense.
- **Attempts expire:** each registration carries a deadline (crawl: the Job's deadline + 1 h; fetch: 24 h). The librarian discards any non-terminal attempt past its deadline. The manager also calls `/discard` as soon as a Job is terminal with nothing staged, and `DELETE /v1/archives/<archive>` discards that archive's attempts.
- **Garbage collection** at startup and hourly: staged and `.part` files whose attempt is unregistered, expired, or terminal for more than 7 days, are deleted. They are the librarian's own temporary files, never served data.
- `status.freeBytes` and `Degraded/LowSpace` (below 2 × `reserveSpace`) report it.

### Crawl attempts

- **Job settings:**
  - `backoffLimit: 0`;
  - `activeDeadlineSeconds` = `timeout` + 6 h post-crawl budget, which covers ZIM conversion and upload, since `--timeSoftLimit` bounds only the crawl;
  - pod `restartPolicy: Never`;
  - `ttlSecondsAfterFinished: 3600`, so the scratch volume, which lives as long as the Pod, is freed within an hour;
  - `terminationMessagePolicy: FallbackToLogsOnError` on both containers.
- **The pod sits in `Pending` for the whole crawl**, because zimit is an init container. The manager reports `Crawling` from the init container's state.
- **zimit arguments:** `--name <archive> --output /output --build /output/build`. `--build` keeps the WARCs in a normal directory instead of zimit's root-only `0700` temp directory, so the non-root upload container can read them. The upload container requires exactly one `*.zim` in `/output` and fails otherwise.
- **Scratch:** a generic ephemeral volume (`scratch.size`), plus a memory `emptyDir` at `/dev/shm`, because Chromium needs more than 64 Mi.
- **`customCss`** exists because the first real archive needed it: the ARM reference's takedown banner and its non-working search box were hidden with CSS. (Pagefind's engine files load only when someone types, so no crawl captures them.) The operator writes it to a per-attempt ConfigMap owned by the Job and passes `--custom-css <path>`.
- **Upload:** streams each part and retries with exponential backoff (capped at 5 min) until the post-crawl budget runs out. The upload container gets `--budget=5h --max-backoff=5m`, and the budget counts only time spent sleeping between retries, not transfer time. Worst-case backoff (5 h 05 m) then leaves about 55 minutes of transfer slack inside the Job's 6 h post-crawl window. A kiwix pod restart, for example during an operator release, therefore doesn't lose a finished crawl.
- **A failed or timed-out attempt** notifies, keeps current, and retries per the due rules.
- **`keepWarcs: true`** registers the attempt with parts `[zim, warcs]`; the upload container sends a tar of only the `build/collections/*/archive` directories (the WARC files) as the `warcs` part. The crawler leaves root-owned `0700` profile directories elsewhere under `build/`, which uid 1000 cannot read, so tarring the whole of `/output/build` would fail; an allowlist also survives any other root-only directory a future zimit leaves behind (`docs/notes/zimit-locked-down.md`).
  - **Promotion:** the tar moves to `/data/.librarian/warcs/<archive>.tar`, replacing the previous one. That's one WARC set per archive, matching the served ZIM, kept outside any glob and never served.
  - **Discard:** a discarded attempt discards its WARCs too.
  - **Disk:** the free-space check counts both parts.
  - **Reading them back:** `GET /v1/archives/<archive>/warcs` streams the tar to a user port-forwarding to the librarian (admin token). zimit's own `--warcs` URL support can't use it directly: it requires a path ending in `.tar`/`.warc(.gz)` and sends no auth. A rebuild then follows the manual procedure above.
  - **When WARCs are removed:** only by `deletionPolicy: Delete`. Turning `keepWarcs` off stops new uploads; the stored tar is kept, because the anti-goal forbids deleting data when a field is removed. `Retain` keeps it.
  - **Why someone would turn it on:** the WARCs let a ZIM be rebuilt with different `warc2zim` options (custom CSS, for example) without crawling the site again, which matters once the site is gone.

### Download attempts

- **`catalog`:**
  - The manager queries `<catalog-url>/catalog/v2/entries?name=<name>&count=-1` (the server returns 10 entries without `count`). The catalog URL is a manager flag, defaulting to `https://opds.library.kiwix.org`.
  - It filters by flavour client-side, because the server ignores `flavour`. `flavour` is required when the name has more than one flavour (otherwise `Degraded/CatalogLookupFailed`); for a name with one flavour (stardew's is empty), it may be omitted.
  - Only a version strictly newer than `current.sourceVersion` proceeds (pre-check above).
  - The `.meta4` gives the URL, size and SHA-256, which are passed in the registration.
- **`url`:** fetched whenever due, unless the `spec.sha256` pre-check says it's unchanged; `sha256` is verified if given.
- **The librarian downloads into staging**, resuming with HTTP Range, following redirects to mirrors, and sending `User-Agent: kiwix-operator/<version>`.

### Notifications

`POST` to `KiwixLibrary.spec.notify.webhook.url` with `{"title","message"}`:
- `arm-reference updated` / `3,540 pages, 48 MB (was 3,531)`
- `arm-reference refresh rejected` / `12 pages vs 3,531 current (needs 80%); kept current`
- `bg3-wiki failed` / `CrawlFailed: <termination message>`. A failure's message is `<Reason>: <message>`, where the reason is the Degraded reason (`CrawlFailed`, `UploadFailed`, `DownloadFailed`, and so on) and the message is the failing container's termination message or the librarian's error.

Delivery is retried (manager flag `--webhook-retries`, default 3), then recorded as a Warning Event; it never blocks reconciliation. The webhook client does not follow redirects, because a followed 301, 302 or 303 would turn the POST into a body-less GET that reports success. A webhook that answers any 3xx therefore counts as a failed delivery. Every outcome is also an Event and a condition. `Unchanged` results never notify.

### Deletion and finalizers

- **A `ZimArchive` finalizer** calls `DELETE /v1/archives/<archive>?policy=…`.
  - `Retain` leaves `/data/<archive>.zim` in place and served. By the name rule it stays reserved for that name, so a re-created archive inherits it.
  - `Delete` removes the book, confirms its absence, and then deletes the file. Until the unlink happens, `DELETE` answers `202` and the finalizer waits, so a `Delete` can't quietly turn into a `Retain` under the name rule. The 10-minute give-up rule below still applies, timed from a stored first-attempt timestamp.
- **If the library is absent, deleting, or its librarian is unreachable for 10 minutes,** the finalizer is removed anyway, the file is left in place, and a Warning Event is recorded.
- **Finalizers need the manager running.** The uninstall docs say to delete `ZimArchive` objects before uninstalling the operator; a namespace deleted after uninstall waits on them.
- **Deleting a `KiwixLibrary`** removes its Deployment, Services, Secrets, NetworkPolicies and HTTPRoute, never the PVC.

### RBAC

Cluster-scoped, like nextdns-operator:
- both CRDs, their `status` and their `finalizers` subresources;
- Deployments, Services, Jobs, Secrets, ConfigMaps (customCss), PVCs, NetworkPolicies;
- Pods (get/list/watch, for termination messages and init-container state);
- Leases (`coordination.k8s.io`, for leader election);
- HTTPRoutes (`gateway.networking.k8s.io`, gateway-api v1.7);
- Events.

The Secret and ConfigMap informers are filtered by the label `app.kubernetes.io/managed-by: kiwix-operator`.

**Images:** the operator image tag defaults to `v` + the chart's `appVersion` (chart `0.1.0` runs image `v0.1.0`; image tags are v-prefixed, chart versions are bare). The librarian and upload containers use the same value, passed to the manager as a flag, so the three can't drift apart. The zimit image is a separate, digest-pinned flag.

## Tests (TDD)

Every behavior lands test-first.
- **Unit:**
  - health gate (ratios, first version, Unchanged);
  - pre-checks (catalog not newer, url sha256 equal);
  - due rules and backoff;
  - catalog parsing and version comparison (flavours, regression, empty flavour, `YYYY-MM` normalization);
  - path-segment and adoptFile name validation;
  - the name rule and manifest rebuild;
  - compare-and-swap;
  - manifest roll-forward for each crash point, using the header UUID;
  - attempt-state transitions, including a lost response and an idle disconnect;
  - mtime bump; Job naming; free-space arithmetic.
- **Controllers, on controller-runtime's fake client with testify** (the nextdns-operator convention; a ruling, because the first draft asked for envtest here):
  - both controllers;
  - attempt idempotency across manager restarts, including between Secret, registration and Job creation;
  - adoption ordering (never fetches while awaiting adoption);
  - Retain then re-create inherits the file;
  - finalizer behavior with a missing or unreachable library;
  - duplicate claim rejection.
  The kind e2e supplies the integration coverage a real API server would, at the cost of weaker integration coverage before it runs.
- **envtest:** CEL validation, defaults and immutability of the two CRDs only.
- **Librarian integration** against a real kiwix-serve from the operator image:
  - zero-book start; startup never blocked by recovery;
  - library of 15+ books with confirmation by book id;
  - rename-over swap with the book id changing;
  - two writes inside one second both confirmed;
  - a write between kiwix-serve's first load and its baseline read, then confirmed by retry;
  - malformed library rejected;
  - `Delete` unlinks only after absence is confirmed;
  - recovery from each crash point;
  - a second librarian refused by the lock.
- **e2e on kind** (isolated kubeconfig, `--crawl-egress-allow-cidrs` set to kind's ranges):
  - a small static site served in-cluster, crawled under the locked-down crawl security settings, with `keepWarcs`;
  - a fake catalog and `.meta4` server for downloads;
  - a webhook sink asserting notifications;
  - a negative test showing a crawl *without* the flag can't reach an in-cluster address.

## How this answers the kiwix-helm-chart review

| Review finding | Resolution here |
|---|---|
| Downloader Job not pinned to the RWO volume's node | Only the kiwix pod mounts the volume |
| Interrupted download leaves a truncated ZIM that's skipped forever | `.part` + SHA-256 + staged rename; Range resume |
| New ZIMs need a pod restart; Reloader annotation is a no-op | `library.xml` + `--monitorLibrary`, every write confirmed by book id |
| First install crash-loops | Librarian writes a zero-book library before kiwix-serve starts |
| CI never runs on PRs; releases never ship | `github-actions-align` standard with release-please |
| `latest` image defaults | Pinned manager flags |
| Flat values block options | Typed CRD fields |
| Removing a URL never removes the file | `deletionPolicy` |
| Job immutability / TTL window | One Job per attempt, found by label; librarian state authoritative |
| One failed download aborts the rest | Each archive is independent |

## Left to the builder

- **To settle in the plan (design gate pass 3, lower severity):** confirmation of adoption by `/content/` link (applied above, test it); the Cilium vs kind NetworkPolicy semantics for `ipBlock` and in-cluster IPs; a watch-topology section (which resources each controller watches and maps back); `Queued` doesn't consume `requestedAt`; DNS-label files that can't be claimed (46–63 characters, or rejected by `kiwix-manage`): as built, only those `kiwix-manage` rejects are listed in `status.unmanagedFiles`, and the 46–63-character ones are served silently.

- Package layout inside `internal/`, and the manifest's JSON shape beyond the fields named here.
- Requeue intervals and the cron library (it must support `timeZone`).
- The e2e harness details.

## Open questions and unverified claims

- Whether `articleCount` tracks HTML pages for every zimit crawl (observed for one crawl). The size ratio is the backstop.
- How warc2zim sets the FRONT_ARTICLE flag.
- Whether headless Chromium's Private Network Access protections add anything beyond the egress policy.
- Whether the cluster is IPv4-only (the NetworkPolicies assume it).
- How Cilium treats kubelet probes under the kiwix pod's ingress policy (`--probe-allow-cidrs` is the escape hatch if they are filtered).
- The OPDS catalog API (`/catalog/v2`) has no published stability promise.
- Native sidecar start ordering is documented Kubernetes behavior, not yet exercised on this cluster.

Closed since the last revision:
- libzim rejects truncated ZIMs on open (libzim 9.5.0 `fileimpl.cpp` L227-231), so `kiwix-manage add` fails on them;
- zimit 3.1.3 runs under the locked-down crawl settings with no capability added back (probe of 2026-11-25, recorded under Crawl pod security).
