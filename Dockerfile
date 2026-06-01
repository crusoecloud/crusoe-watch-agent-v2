FROM golang:1.26-alpine AS builder

RUN apk add --no-cache 'git>=2.47'

ARG CI_SERVER_HOST
ARG CI_JOB_TOKEN

WORKDIR /src
COPY go.mod go.sum ./
RUN go env -w "GOPRIVATE=${CI_SERVER_HOST}" && \
    echo -e "machine ${CI_SERVER_HOST} login gitlab-ci-token password ${CI_JOB_TOKEN}" > ~/.netrc && \
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
