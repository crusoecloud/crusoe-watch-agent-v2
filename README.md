# Crusoe Watch Agent

crusoe-watch-agent is a vector.dev based agent for collecting telemetry data from Crusoe Cloud resources.

Follow the installation instructions below to get started.

## VM

```bash
wget https://github.com/crusoecloud/crusoe-watch-agent-v2/releases/latest/download/crusoe_watch_agent.sh
chmod +x crusoe_watch_agent.sh
sudo ./crusoe_watch_agent.sh install
```

GPU type (NVIDIA or AMD) is auto-detected. The installer runs Vector and the GPU
exporters in Docker by default. Pass `--no-docker` to use native systemd services
instead (NVIDIA only):

```bash
sudo ./crusoe_watch_agent.sh install --no-docker
```

### Monitoring token

Installation requires a monitoring token, which you create with the Crusoe CLI:

```bash
crusoe monitoring tokens create
```

The installer prompts for the token during `install`. To supply it
non-interactively, use the `-f token` flag to print just the token value and
pass it with `--token`:

```bash
sudo ./crusoe_watch_agent.sh install --token "$(crusoe monitoring tokens create -f token)"
```

The token is saved to `/etc/crusoe/secrets/.monitoring-token` and reused by later
runs. Rotate it any time with `refresh-token`.

The chosen mode persists, so later operations use the same script:

```bash
sudo ./crusoe_watch_agent.sh upgrade         # upgrade in place
sudo ./crusoe_watch_agent.sh refresh-token   # rotate the monitoring token
sudo ./crusoe_watch_agent.sh uninstall       # remove (secrets preserved)
sudo ./crusoe_watch_agent.sh help            # all commands and options
```

## Kubernetes (CMK)

Get credentials for your cluster, then install the chart from the OCI registry:

```bash
crusoe kubernetes clusters get-credentials <cluster-name> --project-id <project-id>

helm install crusoe-watch-agent \
  oci://ghcr.io/crusoecloud/crusoe-watch-agent-v2/charts/crusoe-watch-agent \
  --namespace crusoe-system
```

Omitting `--version` installs the latest release. Pin a specific one with
`--version <X.Y>`.

This installs a DaemonSet with one pod per node, running cwa-manager and Vector
side by side. 

### cwa-updater

Agent upgrades are executed in-cluster by cwa-updater, which ships as its own
Helm release and its own version series. Install it into the same namespace, 
ahead of the agent, so the handoff target exists when the agent pods start:

```bash
helm install cwa-updater \
  oci://ghcr.io/crusoecloud/crusoe-watch-agent-v2/charts/cwa-updater \
  --namespace crusoe-system
```

Both charts pull from ghcr.io by default. In clusters without outbound access,
point `agent.chartRepo` at a pull-through OCI mirror.
