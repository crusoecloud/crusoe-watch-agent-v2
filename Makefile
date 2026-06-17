PREFIX ?= $(shell pwd)
MODULE := gitlab.com/crusoeenergy/island/managed-platform-services/crusoe-watch-agent-v2

BUILDDIR := ${PREFIX}/dist
GO_LDFLAGS := -ldflags "-X '${MODULE}/internal/version.Version=$${CI_COMMIT_REF_NAME:-dev}'"

GOLANGCI_VERSION = v1.63.4
GOTESTSUM_VERSION = v1.13.0
GOCOVER_VERSION = v1.4.0
GO_COVER_PACKAGES = ${MODULE}/...

.PHONY: build
build: build-cwa-manager build-mock-coordinator

.PHONY: build-cwa-manager
build-cwa-manager:
	@go build -o ${BUILDDIR}/cwa-manager ${GO_LDFLAGS} ./cmd/cwa-manager

.PHONY: build-mock-coordinator
build-mock-coordinator:
	@go build -o ${BUILDDIR}/mock-coordinator ./cmd/mock-coordinator

.PHONY: cross
cross:
	@GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ${BUILDDIR}/cwa-manager ${GO_LDFLAGS} ./cmd/cwa-manager

.PHONY: cross-mock
cross-mock:
	@GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o ${BUILDDIR}/mock-coordinator ./cmd/mock-coordinator

.PHONY: test
test:
	@go test -v ./...

.PHONY: ci
ci: test-ci build-deps lint-ci

.PHONY: build-deps
build-deps:
	@go install github.com/golangci/golangci-lint/cmd/golangci-lint@${GOLANGCI_VERSION}

.PHONY: test-ci
test-ci:
	@go mod tidy
	@git diff --exit-code go.mod go.sum
	@go install gotest.tools/gotestsum@${GOTESTSUM_VERSION}
	@go install github.com/boumenot/gocover-cobertura@${GOCOVER_VERSION}
	@gotestsum --junitfile tests.xml -- -coverpkg="${GO_COVER_PACKAGES}" -coverprofile=coverage.out -covermode=atomic -race -v ./...
	@go tool cover -func=coverage.out
	@gocover-cobertura < coverage.out > coverage.xml

.PHONY: lint-ci
lint-ci:
	@golangci-lint version
	@golangci-lint run -v ./... --out-format code-climate > golangci-lint.json

.PHONY: lint
lint:
	@golangci-lint run ./...

.PHONY: shellcheck
shellcheck:
	@command -v shellcheck >/dev/null || { echo "install: brew install shellcheck"; exit 1; }
	@shellcheck vm/crusoe_watch_agent.sh

.PHONY: vector-validate
vector-validate: build-cwa-manager
	@command -v vector >/dev/null || { echo "install: brew install vectordotdev/brew/vector  (or see https://vector.dev/docs/setup/installation/)"; exit 1; }
	@mkdir -p ${BUILDDIR}/vector-configs
	@AGENT_VERSION=dev \
	 CRUSOE_AUTH_TOKEN=test-token \
	 CRUSOE_CLUSTER_ID=test-cluster \
	 CRUSOE_MONITORING_TOKEN=test-token \
	 CRUSOE_PROJECT_ID=test-project \
	 LOGS_INGRESS_ENDPOINT=https://example.com \
	 TELEMETRY_INGRESS_ENDPOINT=https://example.com \
	 VM_ID=test-vm \
	 sh -c 'for gpu in none nvidia amd; do \
	    for cme_flag in "" "--cme"; do \
	        suffix=""; [ -n "$$cme_flag" ] && suffix="-cme"; \
	        name="vm-$${gpu}$${suffix}"; \
	        ${BUILDDIR}/cwa-manager --gpu=$$gpu $$cme_flag --dump-vector-config > ${BUILDDIR}/vector-configs/$$name.yaml; \
	        echo "Validating $$name"; \
	        vector validate --no-environment ${BUILDDIR}/vector-configs/$$name.yaml || exit 1; \
	    done; \
	 done'

.PHONY: clean
clean:
	@rm -rf ${BUILDDIR}

.PHONY: run
run: build-cwa-manager
	@${BUILDDIR}/cwa-manager

.PHONY: run-mock
run-mock: build-mock-coordinator
	@${BUILDDIR}/mock-coordinator
