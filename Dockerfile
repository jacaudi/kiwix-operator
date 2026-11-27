FROM golang:1.27.4@sha256:5f1b98fa02fe396ebce2a0227a0e08309e11792915b9dd2e1f71053d7e7e6666 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o kiwix-operator ./cmd

FROM ghcr.io/kiwix/kiwix-tools:3.8.2@sha256:2914a200ff64e0861c82733010eb7583d17334dde4d7248b9d9aa7b2f7b5a36d
COPY --from=builder /workspace/kiwix-operator /usr/local/bin/kiwix-operator
USER 1000:1000
ENTRYPOINT ["/usr/local/bin/kiwix-operator"]
