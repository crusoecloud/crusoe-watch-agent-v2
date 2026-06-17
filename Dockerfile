FROM golang:1.26-alpine AS builder

RUN apk add --no-cache 'git>=2.47'

ARG CI_SERVER_HOST

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=netrc,target=/root/.netrc \
    go env -w "GOPRIVATE=${CI_SERVER_HOST}" && \
    go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X 'gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2/internal/version.Version=${VERSION}'" \
    -o /cwa-manager ./cmd/cwa-manager

FROM alpine:3.21
RUN apk add --no-cache 'ca-certificates>=20250106'
COPY --from=builder /cwa-manager /usr/local/bin/cwa-manager
USER 65534
HEALTHCHECK --interval=30s --timeout=3s CMD ["/usr/local/bin/cwa-manager", "--version"] || exit 1
ENTRYPOINT ["/usr/local/bin/cwa-manager"]
