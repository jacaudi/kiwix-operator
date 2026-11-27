# Archives

This is a task guide to getting content into a library with a `ZimArchive`:
crawling a site, downloading a book, refreshing it, and reading the result.
Field meanings are in [Configuration](configuration.md).

## Crawl a Site

```yaml
apiVersion: kiwix.jacaudi.dev/v1alpha1
kind: ZimArchive
metadata:
  name: go-dev
spec:
  libraryRef:
    name: library
  source:
    crawl:
      seeds:
        - https://go.dev/doc/
      scopeExcludeRx: "^https://go\\.dev/(play|dl)/"
      title: Go documentation
      timeout: 12h
      scratch:
        size: 20Gi
  refresh:
    schedule: "0 3 1 * *"
    timeZone: UTC
  deletionPolicy: Retain
```

Each crawl is one Job on any node. zimit is an init container, so the pod
stays `Pending` for the whole crawl and the archive reports
`Progressing/Crawling`. When zimit finishes, the upload container streams the
ZIM to the library's librarian and retries with backoff if the kiwix pod
restarts meanwhile. Nothing but the kiwix pod mounts the library volume.

- Size `scratch` for the crawl output. zimit exits before fetching anything on
  a volume more than 90% full.
- `timeout` stops the crawl but still builds a ZIM; the health gate catches a
  truncated result.
- The Job's deadline is `timeout` plus 6 hours for conversion and upload.
- Use `customCss` to hide banners or controls that do not work offline.

## Download a Book

```yaml
spec:
  libraryRef:
    name: library
  source:
    download:
      catalog:
        name: wikipedia_en_100
        flavour: full
  refresh:
    schedule: "0 4 * * 0"
```

The manager looks the name up in the catalog (`catalogURL` in the chart). A
newer version is fetched only when it is strictly newer than the current
`sourceVersion`; otherwise the attempt ends `Unchanged`. `flavour` is required
when a name has several; set `source.download.url` instead of `catalog` for a
fixed link, with `sha256` to verify it and to skip an unchanged download.

The librarian downloads into staging, resumes interrupted transfers with HTTP
Range, follows redirects to mirrors, and verifies the SHA-256 from the catalog.

## Refresh and Health Gate

With a `schedule`, the archive refreshes on that cron tick. With none, it is
fetched once and afterwards only on demand:

```bash
kubectl annotate zimarchive go-dev kiwix.jacaudi.dev/requestedAt="$(date -u +%FT%TZ)" --overwrite
```

A new file replaces the current one only if it passes the gate:

- The first version needs `articleCount` of at least 1.
- A refresh needs `articleCount` and size at or above `minArticleRatio` and
  `minSizeRatio` percent of the current version. A takedown page, a login wall
  or a truncated crawl fails this, the result is `Rejected`, the old file stays
  and you are notified.
- To accept a deliberately smaller source, set both ratios to `0` for one
  refresh.

Only one attempt runs per archive at a time. A schedule tick during an attempt
is skipped, and a `requestedAt` change waits for the attempt to end. Without a
current version, a failed attempt is retried after 1 hour, then 4 hours, then
every 24 hours.

## Read the Result

```bash
kubectl get zimarchive
kubectl describe zimarchive go-dev
```

`status.current` is the served version, `status.lastAttempt` the latest
attempt (`result` is `Succeeded`, `Rejected`, `Failed` or `Unchanged`, with a
`reason` and `message`), and `status.nextRefreshAt` the next tick. Events record
each outcome. The reasons are in [Configuration](configuration.md#zimarchive).

## Notifications

Set `spec.notify.webhook.url` on the `KiwixLibrary`. The operator `POST`s
`{"title","message"}` JSON:

| Title | Message |
|---|---|
| `go-dev updated` | `3,540 pages, 48 MB (was 3,531)` |
| `go-dev refresh rejected` | `12 pages vs 3,531 current (needs 80%); kept current` |
| `go-dev failed` | `CrawlFailed: <termination message>` |

Failures and rejections always notify. `spec.notify.onSuccess: true` adds
successes on that archive, and `Unchanged` never notifies. The first failure
notifies at once and repeats during backoff at most once per 24 hours. A failed
delivery is retried (`webhookRetries`), then recorded as a Warning Event. The
client does not follow redirects, so a webhook that answers 3xx counts as
failed.

[← Back to README](../README.md)
