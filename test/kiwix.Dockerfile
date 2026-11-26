# Go toolchain plus the kiwix-tools binaries, for the tests tagged kiwix (task test:kiwix).
FROM ghcr.io/kiwix/kiwix-tools:3.8.2@sha256:2914a200ff64e0861c82733010eb7583d17334dde4d7248b9d9aa7b2f7b5a36d AS kiwix

FROM golang:1.27.4@sha256:5f1b98fa02fe396ebce2a0227a0e08309e11792915b9dd2e1f71053d7e7e6666
COPY --from=kiwix /usr/local/bin/kiwix-serve /usr/local/bin/kiwix-manage /usr/local/bin/
