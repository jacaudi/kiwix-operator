#!/usr/bin/env bash
# Probe: can zimit crawl with all capabilities dropped (plus only $CAPS, space-separated)?
set -euo pipefail

NGINX='nginx:1.29-alpine@sha256:5616878291a2eed594aee8db4dade5878cf7edcb475e59193904b198d9b830de'
ZIMIT='ghcr.io/openzim/zimit:3.1.3@sha256:264d386152a0527d75b562298193d27c7f318ee58eb5191940dc12ad0a1b121b'
NET=kiwix-probe
VOL=kiwix-probe-out
CAPS="${CAPS:-}"
# optional extra zimit args, e.g. "--diskUtilization 0" when the docker VM disk is over 90% full
ZIMIT_EXTRA="${ZIMIT_EXTRA:-}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

cleanup() {
  docker rm -f site zimit-probe >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  docker volume rm -f "$VOL" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup

docker pull -q "$NGINX" >/dev/null
docker pull -q "$ZIMIT" >/dev/null

docker network create "$NET" >/dev/null
docker volume create "$VOL" >/dev/null
# imitate fsGroup: 1000 (root owner, group 1000, setgid)
docker run --rm -v "$VOL":/o "$NGINX" sh -c 'chown 0:1000 /o && chmod 2775 /o'

docker run -d --name site --network "$NET" -v "$HERE/site":/usr/share/nginx/html:ro "$NGINX" >/dev/null

cap_args=()
for c in $CAPS; do cap_args+=(--cap-add "$c"); done

# :nocopy stops docker copying the image's /output ownership onto the volume (k8s does not)
# no --security-opt seccomp=..., so Docker's default profile (RuntimeDefault equivalent)
docker run --name zimit-probe --network "$NET" \
  --cap-drop ALL --security-opt no-new-privileges ${cap_args[@]+"${cap_args[@]}"} \
  -v "$VOL":/output:nocopy "$ZIMIT" \
  zimit --seeds http://site/ --name probe --output /output --build /output/build --workers 1 $ZIMIT_EXTRA

# warcs.tar holds only the WARC archive dirs: the root-owned 0700 Chromium profile elsewhere in build/ is unreadable to uid 1000
docker run --rm --user 1000:1000 -v "$VOL":/output "$NGINX" \
  sh -c 'cd /output && tar -cf warcs.tar build/collections/*/archive'

listing="$(docker run --rm -v "$VOL":/output "$NGINX" sh -c 'ls /output/*.zim 2>/dev/null; echo ---; tar -tf /output/warcs.tar')"
zims="$(printf '%s\n' "$listing" | sed '/^---$/,$d' | grep -c '\.zim$' || true)"
warcs="$(printf '%s\n' "$listing" | sed '1,/^---$/d' | grep -c '\.warc\.gz$' || true)"
if [ "$zims" -ne 1 ] || [ "$warcs" -lt 1 ]; then
  echo "PROBE FAILED: zims=$zims warcs=$warcs caps=[$CAPS]" >&2
  exit 1
fi
echo "PROBE OK: $(basename "$(printf '%s\n' "$listing" | grep '\.zim$')") caps=[$CAPS]"
