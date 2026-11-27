# Architecture

This is an explanation of how the operator is built: the pod layout, who owns
which files, how an attempt runs and how the library survives a crash. For the
short version, see [How It Works](../README.md#how-it-works).

## Processes

One image, `ghcr.io/jacaudi/kiwix-operator`, is built from the pinned
kiwix-tools image plus a Go binary with three subcommands: `manager`,
`librarian` and `upload`. kiwix-serve, `kiwix-manage` and the librarian all run
from it, so they cannot drift apart.

- The **manager** is a Deployment with leader election. It holds policy: the
  schedule, catalog versions, the health gate and notifications.
- The **kiwix pod** is a Deployment of one replica with the `Recreate`
  strategy. The librarian is a native sidecar (an init container with
  `restartPolicy: Always`) and kiwix-serve starts after the librarian's startup
  probe passes. The librarian has no readiness probe, so its health never takes
  kiwix-serve out of its Service.
- A **crawl attempt** is a Job. The zimit init container crawls into an
  ephemeral scratch volume, then the upload container streams the ZIM to the
  librarian.

The librarian is the only writer of `/data`. It holds an exclusive `flock` on
`/data/.librarian/lock` for its whole life and refuses to start without it, and
`/data` is mounted read-only into kiwix-serve. The librarian's HTTP API is the
boundary between policy in the manager and mechanics in the librarian, because
they run in different pods.

## Files and Ownership

`/data/<archive>.zim`, where `<archive>` is a DNS label of at most 45
characters, belongs to the `ZimArchive` of that name, however the file got
there. If it exists and `kiwix-manage add` accepts it, it is that archive's
current version. Every other top-level `*.zim` is unmanaged and served as is.

Two things follow. Re-creating a deleted archive inherits the retained file,
so its next fetch is a gated refresh. And each archive's URL,
`/content/<archive>`, is stable for life.

The librarian keeps a manifest at `/data/.librarian/manifest.json` with every
served book's `<book>` element, each archive's current file, id, counts, hash,
version and source fingerprint, and pending intents and attempts. If it is
lost, it is rebuilt from disk by the name rule, and disk wins over the manifest
when they disagree. Staging (`staged/`) and kept WARC tars (`warcs/`) live under
`/data/.librarian/`, where no glob reaches.

## An Attempt

1. The archive is due: never attempted, a schedule tick, `requestedAt` changed,
   or a retry after backoff.
2. Pre-checks run before anything is downloaded. A catalog version not newer
   than the current one, or a URL whose declared `sha256` equals the current
   file's, ends the attempt `Unchanged`. They apply only when the source
   fingerprint matches the spec. The concurrency limits queue the attempt.
3. The manager registers the attempt with the librarian. A crawl creates a Job;
   a download is fetched by the librarian itself.
4. The librarian stages each part: a `.part` file, free-space checks, SHA-256,
   then `kiwix-manage add` on a scratch library to read the counts.
5. The manager applies the health gate.
6. On a pass the librarian promotes; otherwise the attempt is discarded.
7. The result is recorded in status, an Event and, where due, a notification.

The librarian's attempt state is authoritative: a Job that fails after its
upload completed does not fail the attempt, and an attempt that exceeds its
deadline is discarded. Uploads restart from zero, while downloads resume with
HTTP Range.

## Promotion and Crash Safety

Promotion is a compare-and-swap, so the file replaced is the one the gate
measured. The librarian writes an intent to the manifest, renames the staged
file over `/data/<archive>.zim`, rewrites `library.xml`, then commits. After a
crash, startup rolls the intent forward or drops it.

Every `library.xml` write goes through one routine: render to a temp file,
bump its mtime at least one second past the previous one, rename it over the
file, then confirm each changed book through kiwix-serve's catalog
(`/catalog/v2/entry/<bookId>` is 200 for an added book and 404 for a removed
one). The monitor compares whole-second mtimes, so a write can be missed;
unconfirmed writes are retried up to 3 times and then set
`Degraded/ReloadUnconfirmed`. A deleted file is unlinked only after its
book's absence is confirmed.

On startup the librarian takes the lock, builds or loads the manifest, renders
`library.xml` without confirming, passes its startup probe, and only then
recovers intents and scans. Recovery never blocks kiwix-serve from starting.

Disk space is checked before any part is accepted (`free ≥ size +
reserveSpace`) and while writing. Staged files of unregistered, expired or
long-terminal attempts are garbage-collected at startup and hourly.

## Librarian API

The librarian listens on port 8090. Everything under `/v1/` takes a bearer
token: the manager uses the library's admin token, and a crawl's upload
container uses a token for that one attempt. The probes take none.

| Endpoint | Caller |
|---|---|
| `POST /v1/attempts`, `GET /v1/attempts/<attempt>` | Manager |
| `POST /v1/attempts/<attempt>/promote` and `/discard` | Manager |
| `PUT /v1/attempts/<attempt>/parts/zim` and `/parts/warcs` | Upload container |
| `GET /v1/archives/<archive>`, `POST .../adopt`, `DELETE ...?policy=` | Manager |
| `GET /v1/archives/<archive>/warcs` | Manager, or you via port-forward |
| `GET /v1/library` | Manager |
| `GET /healthz/startup`, `GET /healthz/live` | Kubelet |

[← Back to README](../README.md)
