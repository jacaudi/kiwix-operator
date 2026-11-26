# zimit crawl under dropped capabilities

Probe: `test/zimit/locked-down.sh`, run 2026-11-25 on Docker (colima) against the pinned images.

- zimit: `ghcr.io/openzim/zimit:3.1.3@sha256:264d386152a0527d75b562298193d27c7f318ee58eb5191940dc12ad0a1b121b`
- nginx (site server, volume setup, tar): `nginx:1.29-alpine@sha256:5616878291a2eed594aee8db4dade5878cf7edcb475e59193904b198d9b830de`

## Result

`PROBE OK: probe_2026-11.zim caps=[]` — zimit crawls 3 pages and writes the ZIM with every capability dropped.

**ZimitExtraCapabilities: []** (empty; Task 18 uses `var zimitExtraCapabilities = []corev1.Capability{}`)

## Exact flags

```text
docker run --name zimit-probe --network kiwix-probe \
  --cap-drop ALL --security-opt no-new-privileges \
  -v kiwix-probe-out:/output:nocopy <zimit> \
  zimit --seeds http://site/ --name probe --output /output --build /output/build --workers 1
```

- No `--security-opt seccomp=...`: Docker's default profile, the equivalent of `RuntimeDefault`. Container runs as root (uid 0).
- The volume root is `0:1000`, mode `2775` (imitates `fsGroup: 1000`); the tar step runs `--user 1000:1000` and writes `/output/warcs.tar`.

## Findings that affect later tasks

1. **`:nocopy` is required.** The zimit image bakes in `/output` (`0:0 755`). Docker copies that ownership onto an empty named volume at first mount, which erases the `0:1000 2775` setup and makes the uid-1000 step fail with `tar: can't open '/output/warcs.tar': Permission denied`. Kubernetes does not copy up, so `fsGroup` holds there.
2. **`warcs.tar` must be an allowlist: only `build/collections/*/archive/`.** The crawler leaves `build/collections/<id>/profile` as root-owned `0700` directories. A literal `tar -C /output -cf /output/warcs.tar build` as uid 1000 exits non-zero with 18 `Permission denied` lines (below) even though the ZIM and all `*.warc.gz` files are group/world readable. A denylist of the profile would break again on any other root-only dir a future zimit leaves in `build/`, so the probe (and Task 18) tar just the archive dirs: `sh -c 'cd /output && tar -cf warcs.tar build/collections/*/archive'`. This deviates from the brief's literal command. Capabilities do not help (uid 1000 runs in a different container), as the attempts below show.
3. **Disk threshold.** zimit defaults the crawler to `--diskUtilization 90`; on a Docker VM over 90% full the crawl exits `Out of disk space, exiting` (code 3) before fetching anything. `ZIMIT_EXTRA="--diskUtilization 95"` was passed for these runs (this VM was at 91% after pruning build cache); `0` and `100` did not bypass the check. Not a capability issue.
4. New files inherit group 1000 from the setgid root, so a uid-1000 reader works for ZIM and WARCs.

## Attempts

| CAPS | Outcome |
| --- | --- |
| `""` (literal tar of `build`) | ZIM built; `tar: build/collections/crawl-<id>/profile/...: Permission denied` (18 profile dirs), `tar: error exit delayed from previous errors`; probe exit 1 |
| `SYS_CHROOT` (literal tar) | ZIM built; same `tar ... profile ...: Permission denied` errors; exit 1 |
| `SYS_ADMIN` (literal tar) | ZIM built; same errors; exit 1 |
| `SYS_CHROOT SYS_ADMIN` (literal tar) | ZIM built; same errors; exit 1 |
| `""` (tar of `build/collections/*/archive` only) | `PROBE OK: probe_2026-11.zim caps=[]` |

Earlier failures with `caps=[]` that were environment, not capabilities: `Out of disk space, exiting` (finding 3) and `tar: can't open '/output/warcs.tar': Permission denied` (finding 1).
