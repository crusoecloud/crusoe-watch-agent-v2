# crusoe-watch-agent-v2

Crusoe Watch Agent v2 — Go-based telemetry agent for Crusoe Cloud resources.

[RFC: Crusoe Watch Agent 2.0](https://crusoe.atlassian.net/wiki/spaces/SE/pages/2698248221/RFC+Crusoe+Watch+Agent+2.0+CWA)

## Components

- **cwa-manager**: Heartbeat loop, health collection, command dispatch
- **cwa-updater**: Upgrade execution (planned)
- **Vector**: Data pipeline for metrics and logs (unchanged from v1)

## Build

```bash
make build        # local build
make cross        # linux/amd64 static binaries
make test
make lint
```

## Development

```bash
# Terminal 1: start mock coordinator
make run-mock

# Terminal 2: start cwa-manager
make run
```

## Proto

Regenerate protobuf code after editing `internal/proto/agent.proto`:

```bash
make proto-gen
```
