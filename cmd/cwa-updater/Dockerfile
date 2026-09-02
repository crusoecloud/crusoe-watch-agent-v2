# Mirrors the root cwa-manager Dockerfile: builder pinned to the native build
# host (Go cross-compiles, so `go mod download`/git run natively instead of
# under QEMU, which crashes on large private-module fetches when emulating arm64).
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

RUN apk add --no-cache 'git>=2.47'

ARG CI_SERVER_HOST

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=netrc,target=/root/.netrc \
    go env -w "GOPRIVATE=${CI_SERVER_HOST}" && \
    go mod download

COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags "-s -w -X 'gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version.Version=${VERSION}'" \
    -o /cwa-updater ./cmd/cwa-updater

FROM alpine:3.21
RUN apk add --no-cache 'ca-certificates>=20250106' 'helm>=3.14' 'cosign>=2.2'
COPY --from=builder /cwa-updater /usr/local/bin/cwa-updater
# Public half of the release pipeline's chart signing key. Baked in rather than
# configured, so nothing in the cluster can point verification at another key.
COPY ci/cosign.pub /etc/cwa/cosign.pub
# Matches runAsUser/runAsGroup in the cwa-updater chart; the binary needs no root and writes only to the mounted emptyDir.
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=3s CMD wget -q -O /dev/null "http://127.0.0.1:${CWA_UPDATER_PORT:-8786}/health" || exit 1
ENTRYPOINT ["/usr/local/bin/cwa-updater"]
