# Operations

This is a task guide to running the operator: rebuilding a ZIM by hand, keeping
WARCs, upgrading, deleting objects and uninstalling.

## Rebuilding a ZIM by Hand

The operator does not rebuild ZIMs, for example from kept WARCs. When you build
one yourself, put it in a directory of your own on the same volume, then `mv` it
into place. A rename within one volume is atomic.

```bash
NS=default LIB=library ARCHIVE=go-dev
POD=$(kubectl -n "$NS" get pod \
  -l app.kubernetes.io/name=kiwix,app.kubernetes.io/instance="$LIB" \
  -o jsonpath='{.items[0].metadata.name}')

kubectl -n "$NS" exec "$POD" -c librarian -- mkdir -p /data/.rebuild
kubectl -n "$NS" cp -c librarian ./new.zim "$POD:/data/.rebuild/$ARCHIVE.zim"
kubectl -n "$NS" exec "$POD" -c librarian -- \
  mv "/data/.rebuild/$ARCHIVE.zim" "/data/$ARCHIVE.zim"
kubectl -n "$NS" rollout restart "deployment/$LIB"
```

1. Copy the new file to `/data/.rebuild/`. Wait for the copy to finish and
   leave room for both the old and new file.
2. `mv` it over `/data/<archive>.zim`, where `<archive>` is the `ZimArchive`
   name.
3. Restart the kiwix pod so the librarian's startup scan picks the file up. The
   scan reads each file's header UUID, and the disk wins over the manifest.

Never use `/data/.librarian/staged/` for this. The librarian's garbage
collection deletes any file there that belongs to no registered attempt, at
startup and every hour, so a copy in progress can be lost. `kubectl cp`
straight to `/data/<archive>.zim` is a non-atomic write and is not supported.

## Keeping and Reading WARCs

`crawl.keepWarcs: true` uploads a tar of the crawl's WARC directories with the
ZIM. It is stored at `/data/.librarian/warcs/<archive>.tar`, one set per
archive, replacing the previous one. It is never served. With the WARCs you can
rebuild a ZIM with different `warc2zim` options after the site is gone.

To read the tar, port-forward to the librarian and use the admin token:

```bash
kubectl -n "$NS" port-forward "svc/$LIB-librarian" 8090:8090 &
TOKEN=$(kubectl -n "$NS" get secret "$LIB-librarian-admin" \
  -o jsonpath='{.data.token}' | base64 -d)
curl -H "Authorization: Bearer $TOKEN" -o "$ARCHIVE-warcs.tar" \
  "http://localhost:8090/v1/archives/$ARCHIVE/warcs"
```

Then rebuild by hand as above. **The tar is removed only by
`deletionPolicy: Delete`.** Turning `keepWarcs` off stops new uploads but keeps
the stored tar, and `Retain` keeps it too.

## Upgrading

- **The operator.** Every release changes the image, which recreates each kiwix
  pod and briefly interrupts kiwix-serve. A crawl upload in progress keeps
  retrying with backoff, so a finished crawl is not lost.
- **The CRDs.** Helm installs the chart's `crds/` directory on first install
  only and never upgrades it. Apply new CRDs yourself after a chart upgrade:

  ```bash
  helm show crds oci://ghcr.io/jacaudi/charts/kiwix-operator \
    --version <version> | kubectl apply --server-side -f -
  ```

- **Chart settings.** Do not set `global.nameOverride`; the library's
  NetworkPolicy selects the manager by its `app.kubernetes.io/name` label.

## Deleting Archives and Libraries

A `ZimArchive` finalizer asks the librarian to apply `deletionPolicy`:

- `Retain` leaves `/data/<archive>.zim` in place and served. The name stays
  reserved, so a re-created archive inherits the file.
- `Delete` removes the book, confirms kiwix-serve no longer serves it, then
  deletes the file and the kept WARC tar. Until then the finalizer waits.

If the library is absent or deleting, or its librarian has been unreachable for
10 minutes, the finalizer is removed anyway, the file is left in place and a
Warning Event is recorded.

Deleting a `KiwixLibrary` removes its Deployment, Services, Secret,
NetworkPolicies and HTTPRoute, never the PVC. Re-creating a library of the same
name reuses `<library>-data`.

## Uninstalling

1. Delete every `ZimArchive` while the operator still runs. The finalizers need
   the manager, and a namespace deleted after the uninstall waits on them.

   ```bash
   kubectl delete zimarchive --all --all-namespaces
   ```

2. Delete the `KiwixLibrary` objects.
3. Uninstall the chart: `helm uninstall kiwix-operator -n kiwix-operator`.

Helm leaves the CRDs, and the PVCs keep your ZIM files.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Library `Degraded/ClaimInUse` | Another library names the same claim; the older one wins |
| Library `Degraded/ReloadUnconfirmed` | kiwix-serve did not confirm a `library.xml` rewrite; the files are in place and the next write or a restart reloads |
| Library `Degraded/RouteUnsupported` | The Gateway API CRDs are not installed; kiwix-serve keeps serving |
| Library `Degraded/LowSpace` | Free space is below twice `reserveSpace` |
| Archive `Degraded/InsufficientSpace` | The volume lacks room for the new file plus `reserveSpace` |
| Archive `Degraded/GateRejected` | The new file was too small; see [Archives](archives.md#refresh-and-health-gate) |
| Manager cannot reach the librarian | Check `global.nameOverride` is unset, and `probeAllowCIDRs` if your CNI filters kubelet probes |

[← Back to README](../README.md)
