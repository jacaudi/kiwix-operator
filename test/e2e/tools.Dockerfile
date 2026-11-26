# The e2e fake catalog and webhook sink, built and run on the pinned Go image, with the book the fake catalog serves.
FROM golang:1.27.4@sha256:5f1b98fa02fe396ebce2a0227a0e08309e11792915b9dd2e1f71053d7e7e6666 AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
COPY test/e2e/ test/e2e/
RUN CGO_ENABLED=0 go build -trimpath -o /out/ ./test/e2e/fakecatalog ./test/e2e/sink

FROM golang:1.27.4@sha256:5f1b98fa02fe396ebce2a0227a0e08309e11792915b9dd2e1f71053d7e7e6666
COPY --from=builder /out/ /usr/local/bin/
COPY test/fixtures/zim/book-01.zim /srv/book-01.zim
USER 65532:65532
