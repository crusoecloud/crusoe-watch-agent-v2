# Pin the builder to the native build host (not the target arch): Go cross-compiles,
# so `go mod download`/git run natively instead of under QEMU, which crashes on large
# private-module fetches (index-pack "Bad address") when emulating arm64.
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
    -o /cwa-manager ./cmd/cwa-manager

FROM alpine:3.21
RUN apk add --no-cache 'ca-certificates>=20250106'
COPY --from=builder /cwa-manager /usr/local/bin/cwa-manager
ENTRYPOINT ["/usr/local/bin/cwa-manager"]
